package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"filetrans-backend/internal/config"

	"github.com/gofiber/fiber/v2"
)

type uploadPolicyRequest struct {
	AllowedExtensions []string `json:"allowedExtensions"`
	BlockedExtensions []string `json:"blockedExtensions"`
}

type uploadPolicyResponse struct {
	AllowedExtensions []string `json:"allowedExtensions"`
	BlockedExtensions []string `json:"blockedExtensions"`
}

type filePickerRootDTO struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	AllowSelectFiles bool   `json:"allowSelectFiles"`
	AllowSelectDirs  bool   `json:"allowSelectDirs"`
}

type filePickerItemDTO struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       *int64 `json:"size"`
	ModifiedAt string `json:"modifiedAt,omitempty"`
	Hidden     bool   `json:"hidden"`
	Symlink    bool   `json:"symlink"`
	Selectable bool   `json:"selectable"`
	Readable   bool   `json:"readable"`
}

type filePickerListResponse struct {
	RootID     string              `json:"rootId"`
	Path       string              `json:"path"`
	ParentPath string              `json:"parentPath"`
	Page       int                 `json:"page"`
	PageSize   int                 `json:"pageSize"`
	HasMore    bool                `json:"hasMore"`
	Items      []filePickerItemDTO `json:"items"`
}

type filePickerValidateRequest struct {
	RootID       string `json:"rootId"`
	Path         string `json:"path"`
	ExpectedType string `json:"expectedType"`
}

type filePickerValidateResponse struct {
	Valid        bool   `json:"valid"`
	RootID       string `json:"rootId"`
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	Type         string `json:"type"`
	AbsolutePath string `json:"absolutePath"`
}

type resolvedPickerPath struct {
	root         config.FilePickerRoot
	rootReal     string
	relativePath string
	virtualPath  string
	absolutePath string
}

func (s *Server) filePickerRoots(c *fiber.Ctx) error {
	cfg := s.cfg()
	out := make([]filePickerRootDTO, 0, len(cfg.FilePicker.Roots))
	for _, root := range cfg.FilePicker.Roots {
		out = append(out, filePickerRootDTO{ID: root.ID, Name: root.Name, AllowSelectFiles: root.AllowSelectFiles, AllowSelectDirs: root.AllowSelectDirs})
	}
	return c.JSON(out)
}

func (s *Server) filePickerList(c *fiber.Ctx) error {
	rootID := strings.TrimSpace(c.Query("rootId"))
	page := c.QueryInt("page", 1)
	if page < 1 {
		page = 1
	}
	maxPageSize := s.cfg().FilePicker.MaxPageSize
	pageSize := clampPositive(c.QueryInt("pageSize", maxPageSize), maxPageSize)
	resolved, err := s.resolvePickerPath(rootID, c.Query("path"))
	if err != nil {
		_ = s.store.Audit("file_picker_denied", s.clientIP(c), rootID+":"+c.Query("path"))
		return err
	}
	info, err := os.Stat(resolved.absolutePath)
	if err != nil {
		return friendlyPathError(err, "目录不存在或不可访问。")
	}
	if !info.IsDir() {
		return fiber.NewError(fiber.StatusBadRequest, "只能浏览目录。")
	}
	items, hasMore, err := s.readPickerDir(resolved, page, pageSize)
	if err != nil {
		return friendlyPathError(err, "目录无法读取，请检查服务端权限。")
	}
	return c.JSON(filePickerListResponse{RootID: resolved.root.ID, Path: resolved.virtualPath, ParentPath: pickerParentPath(resolved.virtualPath), Page: page, PageSize: pageSize, HasMore: hasMore, Items: items})
}

func (s *Server) filePickerValidate(c *fiber.Ctx) error {
	var in filePickerValidateRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	resolved, err := s.resolvePickerPath(in.RootID, in.Path)
	if err != nil {
		_ = s.store.Audit("file_picker_denied", s.clientIP(c), in.RootID+":"+in.Path)
		return err
	}
	info, err := os.Stat(resolved.absolutePath)
	if err != nil {
		return friendlyPathError(err, "路径不存在，请刷新后重试。")
	}
	pickedType := config.ResourceFile
	if info.IsDir() {
		pickedType = config.ResourceDirectory
	}
	expected := strings.ToLower(strings.TrimSpace(in.ExpectedType))
	if expected == "dir" || expected == "folder" {
		expected = config.ResourceDirectory
	}
	if expected == "" {
		expected = pickedType
	}
	if expected != pickedType {
		return fiber.NewError(fiber.StatusBadRequest, "选择结果类型与当前资源类型不一致。")
	}
	if pickedType == config.ResourceFile && !resolved.root.AllowSelectFiles || pickedType == config.ResourceDirectory && !resolved.root.AllowSelectDirs {
		return fiber.NewError(fiber.StatusForbidden, "当前根目录不允许选择该类型资源。")
	}
	_ = s.store.Audit("file_picker_select", s.clientIP(c), fmt.Sprintf("%s:%s", resolved.root.ID, resolved.virtualPath))
	return c.JSON(filePickerValidateResponse{Valid: true, RootID: resolved.root.ID, Path: resolved.virtualPath, RelativePath: resolved.relativePath, Type: pickedType, AbsolutePath: resolved.absolutePath})
}

func (s *Server) readPickerDir(resolved resolvedPickerPath, page, pageSize int) ([]filePickerItemDTO, bool, error) {
	dir, err := os.Open(resolved.absolutePath)
	if err != nil {
		return nil, false, err
	}
	defer dir.Close()
	start := (page - 1) * pageSize
	items := make([]filePickerItemDTO, 0, pageSize)
	accepted := 0
	for {
		// 使用流式分批读取，避免像普通文件列表那样一次性把超大目录全部读入内存。
		entries, err := dir.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		for _, entry := range entries {
			item, include := s.pickerItem(resolved, entry)
			if !include {
				continue
			}
			if accepted < start {
				accepted++
				continue
			}
			if len(items) >= pageSize {
				return items, true, nil
			}
			items = append(items, item)
			accepted++
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return items, false, nil
}

func (s *Server) pickerItem(resolved resolvedPickerPath, entry os.DirEntry) (filePickerItemDTO, bool) {
	name := entry.Name()
	hidden := strings.HasPrefix(name, ".")
	if s.pickerNameDenied(resolved.root, name, hidden) {
		return filePickerItemDTO{}, false
	}
	symlink := entry.Type()&os.ModeSymlink != 0
	entryPath := filepath.Join(resolved.absolutePath, name)
	rel := filepath.ToSlash(filepath.Join(resolved.relativePath, name))
	info, err := os.Lstat(entryPath)
	readable := err == nil
	itemType := "other"
	var size *int64
	modifiedAt := ""
	if err == nil {
		modifiedAt = info.ModTime().Format(time.RFC3339)
		if info.IsDir() {
			itemType = config.ResourceDirectory
		} else if symlink {
			itemType = "symlink"
		} else if info.Mode().IsRegular() {
			itemType = config.ResourceFile
			value := info.Size()
			size = &value
		}
	}
	if symlink && resolved.root.FollowSymlinks {
		if targetInfo, statErr := os.Stat(entryPath); statErr == nil {
			itemType = config.ResourceFile
			if targetInfo.IsDir() {
				itemType = config.ResourceDirectory
			} else {
				value := targetInfo.Size()
				size = &value
			}
			modifiedAt = targetInfo.ModTime().Format(time.RFC3339)
		}
	}
	selectable := readable && !symlink && (itemType == config.ResourceFile && resolved.root.AllowSelectFiles || itemType == config.ResourceDirectory && resolved.root.AllowSelectDirs)
	return filePickerItemDTO{Name: name, Path: "/" + rel, Type: itemType, Size: size, ModifiedAt: modifiedAt, Hidden: hidden, Symlink: symlink, Selectable: selectable, Readable: readable}, true
}

func (s *Server) resolvePickerPath(rootID, virtualPath string) (resolvedPickerPath, error) {
	root, ok := s.filePickerRoot(rootID)
	if !ok {
		return resolvedPickerPath{}, fiber.NewError(fiber.StatusNotFound, "文件选择器根目录不存在。")
	}
	rootAbs, err := filepath.Abs(root.Path)
	if err != nil {
		return resolvedPickerPath{}, err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return resolvedPickerPath{}, friendlyPathError(err, "文件选择器根目录不存在或不可访问。")
	}
	rootReal, err = filepath.Abs(rootReal)
	if err != nil {
		return resolvedPickerPath{}, err
	}
	if isDangerousRoot(rootReal) {
		return resolvedPickerPath{}, fiber.NewError(fiber.StatusBadRequest, "文件选择器根目录不能是系统根目录或关键系统目录。")
	}
	rel, err := cleanPickerVirtualPath(virtualPath)
	if err != nil {
		return resolvedPickerPath{}, err
	}
	target := filepath.Join(rootReal, filepath.FromSlash(rel))
	if err := ensurePathInside(rootReal, target); err != nil {
		return resolvedPickerPath{}, fiber.NewError(fiber.StatusForbidden, "路径超出文件选择器根目录。")
	}
	if !root.FollowSymlinks && rel != "" {
		// 文件选择器默认不跟随符号链接，避免管理员在弹窗内误入指向系统目录的链接。
		if hasSymlinkAncestor(rootReal, rel) {
			return resolvedPickerPath{}, fiber.NewError(fiber.StatusForbidden, "默认不允许进入符号链接路径。")
		}
	} else if root.FollowSymlinks {
		realTarget, err := filepath.EvalSymlinks(target)
		if err == nil {
			if err := ensurePathInside(rootReal, realTarget); err != nil {
				return resolvedPickerPath{}, fiber.NewError(fiber.StatusForbidden, "符号链接目标超出文件选择器根目录。")
			}
			target = realTarget
		}
	}
	return resolvedPickerPath{root: root, rootReal: rootReal, relativePath: rel, virtualPath: "/" + rel, absolutePath: target}, nil
}

func (s *Server) filePickerRoot(id string) (config.FilePickerRoot, bool) {
	for _, root := range s.cfg().FilePicker.Roots {
		if root.ID == id {
			return root, true
		}
	}
	return config.FilePickerRoot{}, false
}

func cleanPickerVirtualPath(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" || input == "/" || input == "." {
		return "", nil
	}
	for _, r := range input {
		if r == 0 || unicode.IsControl(r) {
			return "", fiber.NewError(fiber.StatusBadRequest, "路径包含非法控制字符。")
		}
	}
	if strings.Contains(input, "\\") || strings.HasPrefix(input, "//") || hasWindowsDrivePrefix(input) {
		return "", fiber.NewError(fiber.StatusBadRequest, "文件选择器路径只能使用根内的 / 分隔相对路径。")
	}
	for _, part := range strings.Split(strings.Trim(input, "/"), "/") {
		if part == ".." {
			return "", fiber.NewError(fiber.StatusBadRequest, "路径不能包含上级目录跳转。")
		}
	}
	cleaned := path.Clean("/" + input)
	rel := strings.TrimPrefix(cleaned, "/")
	if rel == "." || rel == "" {
		return "", nil
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fiber.NewError(fiber.StatusBadRequest, "路径不能包含上级目录跳转。")
	}
	return rel, nil
}

func hasWindowsDrivePrefix(value string) bool {
	return len(value) >= 2 && value[1] == ':' && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z'))
}

func ensurePathInside(baseReal, target string) error {
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(baseReal, targetAbs)
	if err != nil {
		return err
	}
	if rel == "." || rel == "" || rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel) {
		return nil
	}
	return os.ErrPermission
}

func hasSymlinkAncestor(rootReal, rel string) bool {
	current := rootReal
	for _, part := range strings.Split(rel, "/") {
		if part == "" {
			continue
		}
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if err != nil {
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

func (s *Server) pickerNameDenied(root config.FilePickerRoot, name string, hidden bool) bool {
	lower := strings.ToLower(name)
	if hidden && !root.ShowHidden {
		return true
	}
	cfg := s.cfg().FilePicker
	for _, deny := range cfg.DenyNames {
		if lower == deny {
			return true
		}
	}
	for _, pattern := range cfg.DenyPatterns {
		if ok, _ := filepath.Match(pattern, lower); ok {
			return true
		}
	}
	return false
}

func pickerParentPath(value string) string {
	cleaned := path.Clean("/" + strings.TrimSpace(value))
	if cleaned == "/" || cleaned == "." {
		return ""
	}
	parent := path.Dir(cleaned)
	if parent == "." {
		return "/"
	}
	return parent
}

func clampPositive(value, max int) int {
	if value < 1 {
		return 1
	}
	if max > 0 && value > max {
		return max
	}
	return value
}
