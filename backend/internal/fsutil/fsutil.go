package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrUnsafePath 表示用户输入试图越过配置目录边界，例如绝对路径、NUL 或 ..。
var ErrUnsafePath = errors.New("unsafe path")

type Entry struct {
	Name       string `json:"name"`
	IsDir      bool   `json:"isDir"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modifiedAt"`
	Path       string `json:"path"`
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
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

func CleanRel(rel string) (string, error) {
	if rel == "" || rel == "." {
		return "", nil
	}
	if filepath.IsAbs(rel) || strings.Contains(rel, "\x00") {
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

func List(base, rel string) ([]Entry, error) {
	full, safeRel, err := Resolve(base, rel)
	if err != nil {
		return nil, err
	}
	infos, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(infos))
	for _, de := range infos {
		info, err := de.Info()
		if err != nil {
			return nil, err
		}
		p := filepath.ToSlash(filepath.Join(safeRel, de.Name()))
		out = append(out, Entry{
			Name:       de.Name(),
			IsDir:      de.IsDir(),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().Format(time.RFC3339),
			Path:       p,
		})
	}
	return out, nil
}

func UniquePath(dir, filename string) string {
	name := SafeName(filename)
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
		return candidate
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		// 同名文件自动追加 -1、-2，配合 O_EXCL 写入可避免并发上传互相覆盖。
		candidate = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}

func realBase(base string) (string, error) {
	if err := os.MkdirAll(base, 0755); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	return filepath.Abs(real)
}

func ensureRealPathInside(baseReal, target string) error {
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	realTarget, err = filepath.Abs(realTarget)
	if err != nil {
		return err
	}
	if realTarget == baseReal || strings.HasPrefix(realTarget, baseReal+string(os.PathSeparator)) {
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
