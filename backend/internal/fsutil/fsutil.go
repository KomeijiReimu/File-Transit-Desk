package fsutil

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ErrUnsafePath 表示用户输入试图越过配置目录边界，例如绝对路径、NUL 或 ..。
var ErrUnsafePath = errors.New("unsafe path")

type Entry struct {
	Name          string `json:"name"`
	IsDir         bool   `json:"isDir"`
	Size          int64  `json:"size"`
	ModifiedAt    string `json:"modifiedAt"`
	Path          string `json:"path"`
	Type          string `json:"type"`
	MetadataKnown bool   `json:"metadataKnown"`
	Downloadable  bool   `json:"downloadable"`
}

var ErrPageOutOfRange = errors.New("directory page is outside scan window")

type DirectoryReader interface {
	ReadDir(n int) ([]os.DirEntry, error)
	Close() error
}

type ListOptions struct {
	ScanLimit int
	Page      int64
	PageSize  int64
	OpenDir   func(string) (DirectoryReader, error)
}

type ListResult struct {
	Entries        []Entry
	Page           int64
	PageSize       int64
	HasMore        bool
	Truncated      bool
	TotalKnown     bool
	Total          *int64
	ScannedEntries int
	ScanLimit      int
}

func SafeName(name string) string {
	// 上传文件名只保留最后一级名称，并移除控制字符和路径分隔符，避免客户端伪造目录结构。
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, name)
	// Windows 和部分文件系统会忽略尾随空格/点，先规范化再做扩展名校验，避免 bad.exe 之类绕过。
	name = strings.TrimRight(strings.TrimSpace(name), ". ")
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

func CleanRel(rel string) (string, error) {
	if rel == "" || rel == "." {
		return "", nil
	}
	if strings.Contains(rel, "\x00") || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || hasPortableWindowsVolume(rel) {
		return "", ErrUnsafePath
	}
	// 同时接受 / 和 \，但最终统一为 slash 形式，保证前端和数据库里的路径表现一致。
	parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
	cleanParts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", ErrUnsafePath
		}
		cleanParts = append(cleanParts, part)
	}
	return filepath.ToSlash(filepath.Join(cleanParts...)), nil
}

func hasPortableWindowsVolume(path string) bool {
	return len(path) >= 2 && ((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) && path[1] == ':'
}

func Canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	current := abs
	suffix := make([]string, 0)
	for {
		real, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			real, err = filepath.Abs(real)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return filepath.Clean(real), nil
		}
		if !os.IsNotExist(evalErr) {
			return "", evalErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", evalErr
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func IsInside(base, target string) (bool, error) {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return false, err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	baseVolume, targetVolume := filepath.VolumeName(baseAbs), filepath.VolumeName(targetAbs)
	if baseVolume != "" && targetVolume != "" && !strings.EqualFold(baseVolume, targetVolume) {
		return false, nil
	}
	baseReal, err := Canonical(base)
	if err != nil {
		return false, err
	}
	targetReal, err := Canonical(target)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(baseReal, targetReal)
	if err != nil {
		return false, err
	}
	if rel == "." || rel == "" {
		return true, nil
	}
	if filepath.IsAbs(rel) {
		return false, nil
	}
	parts := strings.Split(filepath.Clean(rel), string(os.PathSeparator))
	return len(parts) == 0 || parts[0] != "..", nil
}

func SamePath(left, right string) (bool, error) {
	leftAbs, err := filepath.Abs(left)
	if err != nil {
		return false, err
	}
	rightAbs, err := filepath.Abs(right)
	if err != nil {
		return false, err
	}
	leftVolume, rightVolume := filepath.VolumeName(leftAbs), filepath.VolumeName(rightAbs)
	if leftVolume != "" && rightVolume != "" && !strings.EqualFold(leftVolume, rightVolume) {
		return false, nil
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo), nil
	}
	if leftErr != nil && !os.IsNotExist(leftErr) {
		return false, leftErr
	}
	if rightErr != nil && !os.IsNotExist(rightErr) {
		return false, rightErr
	}
	leftCanonical, err := Canonical(left)
	if err != nil {
		return false, err
	}
	rightCanonical, err := Canonical(right)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(leftCanonical, rightCanonical)
	if err == nil && rel == "." {
		return true, nil
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(leftCanonical), filepath.Clean(rightCanonical)), nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func Resolve(base, rel string) (string, string, error) {
	baseReal, err := realBase(base)
	if err != nil {
		return "", "", err
	}
	safeRel, err := CleanRel(rel)
	if err != nil {
		return "", "", err
	}
	full := filepath.Join(baseReal, filepath.FromSlash(safeRel))
	// 已存在目标必须解析真实路径后仍位于根目录内，阻止符号链接逃逸。
	if err := ensureRealPathInside(baseReal, full); err != nil {
		return "", "", err
	}
	return full, safeRel, nil
}

func ResolveForCreate(base, rel string) (string, string, error) {
	baseReal, err := realBase(base)
	if err != nil {
		return "", "", err
	}
	safeRel, err := CleanRel(rel)
	if err != nil {
		return "", "", err
	}
	full := filepath.Join(baseReal, filepath.FromSlash(safeRel))
	// 新建路径还不存在时，只能校验最近存在父目录，避免上传到穿出根目录的符号链接下。
	parent := nearestExistingParent(full)
	if err := ensureRealPathInside(baseReal, parent); err != nil {
		return "", "", err
	}
	return full, safeRel, nil
}

func EnsureInside(base, target string) error {
	baseReal, err := realBase(base)
	if err != nil {
		return err
	}
	// 创建目录之后再次解析真实路径，缩小“检查后被替换为符号链接”的竞态窗口。
	return ensureRealPathInside(baseReal, target)
}

func ListDirectory(base, rel string, options ListOptions) (ListResult, error) {
	if options.ScanLimit < 1 || options.Page < 1 || options.PageSize < 1 {
		return ListResult{}, ErrPageOutOfRange
	}
	pageIndex := options.Page - 1
	if pageIndex > int64(^uint64(0)>>1)/options.PageSize || pageIndex > int64(options.ScanLimit)/options.PageSize {
		return ListResult{}, ErrPageOutOfRange
	}
	offset := pageIndex * options.PageSize
	if offset >= int64(options.ScanLimit) {
		return ListResult{}, ErrPageOutOfRange
	}
	full, safeRel, err := Resolve(base, rel)
	if err != nil {
		return ListResult{}, err
	}
	opener := options.OpenDir
	if opener == nil {
		opener = func(path string) (DirectoryReader, error) { return os.Open(path) }
	}
	dir, err := opener(full)
	if err != nil {
		return ListResult{}, err
	}
	defer dir.Close()
	entries, readErr := dir.ReadDir(options.ScanLimit + 1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return ListResult{}, readErr
	}
	truncated := len(entries) > options.ScanLimit
	if truncated {
		entries = entries[:options.ScanLimit]
	}
	scannedEntries := len(entries)
	out := make([]Entry, 0, len(entries))
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), ".upload-") && strings.HasSuffix(de.Name(), ".tmp") {
			// 上传临时文件不属于已提交文件，避免长上传期间被列表或下载入口提前暴露。
			continue
		}
		p := filepath.ToSlash(filepath.Join(safeRel, de.Name()))
		entry := Entry{Name: de.Name(), Path: p, Type: "unknown"}
		info, infoErr := de.Info()
		if infoErr == nil {
			entry.MetadataKnown = true
			entry.Size = info.Size()
			entry.ModifiedAt = info.ModTime().Format(time.RFC3339)
			entry.IsDir = info.IsDir()
			switch {
			case info.IsDir():
				entry.Type = "directory"
			case info.Mode()&os.ModeSymlink != 0:
				entry.Type = "symlink"
				targetPath := filepath.Join(full, de.Name())
				if target, statErr := os.Stat(targetPath); statErr == nil && target.Mode().IsRegular() {
					if inside, insideErr := IsInside(base, targetPath); insideErr == nil && inside {
						entry.Downloadable = true
					}
				}
			case info.Mode().IsRegular():
				entry.Type = "file"
				entry.Downloadable = true
			default:
				entry.Type = "other"
			}
		}
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		left, right := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if left == right {
			return out[i].Name < out[j].Name
		}
		return left < right
	})
	totalKnown := !truncated
	var total *int64
	if totalKnown {
		value := int64(len(out))
		total = &value
	}
	start := offset
	if start > int64(len(out)) {
		start = int64(len(out))
	}
	end := start + options.PageSize
	if end < start || end > int64(len(out)) {
		end = int64(len(out))
	}
	pageEntries := append([]Entry(nil), out[start:end]...)
	return ListResult{Entries: pageEntries, Page: options.Page, PageSize: options.PageSize, HasMore: end < int64(len(out)), Truncated: truncated, TotalKnown: totalKnown, Total: total, ScannedEntries: scannedEntries, ScanLimit: options.ScanLimit}, nil
}

func realBase(base string) (string, error) {
	return Canonical(base)
}

func ensureRealPathInside(baseReal, target string) error {
	inside, err := IsInside(baseReal, target)
	if err != nil {
		return err
	}
	if inside {
		return nil
	}
	return ErrUnsafePath
}

func nearestExistingParent(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}
