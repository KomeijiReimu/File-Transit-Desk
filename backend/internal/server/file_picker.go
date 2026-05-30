package server

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
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
	Sort       string              `json:"sort"`
	Order      string              `json:"order"`
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
	roots := s.filePickerRootsForRuntime()
	out := make([]filePickerRootDTO, 0, len(roots))
	for _, root := range roots {
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
	sortBy := normalizePickerSort(c.Query("sort", "name"))
	order := normalizePickerOrder(c.Query("order", "asc"))
	resolved, err := s.resolvePickerPath(rootID, c.Query("path"))
	if err != nil {
		_ = s.store.Audit("file_picker_denied", s.clientIP(c), fmt.Sprintf("根 %s 路径校验失败", rootID))
		return err
	}
	info, err := os.Stat(resolved.absolutePath)
	if err != nil {
		return friendlyPathError(err, "目录不存在或不可访问。")
	}
	if !info.IsDir() {
		return fiber.NewError(fiber.StatusBadRequest, "只能浏览目录。")
	}
	items, hasMore, err := s.readPickerDir(resolved, page, pageSize, sortBy, order)
	if err != nil {
		return friendlyPathError(err, "目录无法读取，请检查服务端权限。")
	}
	return c.JSON(filePickerListResponse{RootID: resolved.root.ID, Path: resolved.virtualPath, ParentPath: pickerParentPath(resolved.virtualPath), Sort: sortBy, Order: order, Page: page, PageSize: pageSize, HasMore: hasMore, Items: items})
}

func (s *Server) filePickerValidate(c *fiber.Ctx) error {
	var in filePickerValidateRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	resolved, err := s.resolvePickerPath(in.RootID, in.Path)
	if err != nil {
		_ = s.store.Audit("file_picker_denied", s.clientIP(c), fmt.Sprintf("根 %s 选择校验失败", in.RootID))
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

func (s *Server) readPickerDir(resolved resolvedPickerPath, page, pageSize int, sortBy, order string) ([]filePickerItemDTO, bool, error) {
	entries, err := os.ReadDir(resolved.absolutePath)
	if err != nil {
		return nil, false, err
	}
	all := make([]filePickerItemDTO, 0, len(entries))
	for _, entry := range entries {
		item, include := s.pickerItem(resolved, entry)
		if include {
			all = append(all, item)
		}
	}
	sortPickerItems(all, sortBy, order)
	start := (page - 1) * pageSize
	if start >= len(all) {
		return []filePickerItemDTO{}, false, nil
	}
	end := start + pageSize
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], end < len(all), nil
}

func sortPickerItems(items []filePickerItemDTO, sortBy, order string) {
	desc := order == "desc"
	sort.SliceStable(items, func(i, j int) bool {
		left, right := items[i], items[j]
		// 商用文件选择器默认遵循常见系统文件浏览器习惯：目录始终排在文件前面。
		if leftRank, rightRank := pickerTypeRank(left), pickerTypeRank(right); leftRank != rightRank {
			return leftRank < rightRank
		}
		less := pickerItemLess(left, right, sortBy)
		if desc {
			return !less && !pickerItemEqual(left, right, sortBy)
		}
		return less
	})
}

func pickerItemLess(left, right filePickerItemDTO, sortBy string) bool {
	switch sortBy {
	case "type":
		if pickerTypeRank(left) != pickerTypeRank(right) {
			return pickerTypeRank(left) < pickerTypeRank(right)
		}
	case "size":
		leftSize, rightSize := pickerSizeValue(left), pickerSizeValue(right)
		if leftSize != rightSize {
			return leftSize < rightSize
		}
	case "modifiedAt":
		leftTime, rightTime := pickerTimeValue(left), pickerTimeValue(right)
		if !leftTime.Equal(rightTime) {
			return leftTime.Before(rightTime)
		}
	}
	return strings.ToLower(left.Name) < strings.ToLower(right.Name)
}

func pickerItemEqual(left, right filePickerItemDTO, sortBy string) bool {
	return !pickerItemLess(left, right, sortBy) && !pickerItemLess(right, left, sortBy)
}

func pickerTypeRank(item filePickerItemDTO) int {
	switch item.Type {
	case config.ResourceDirectory:
		return 0
	case config.ResourceFile:
		return 1
	case "symlink":
		return 2
	default:
		return 3
	}
}

func pickerSizeValue(item filePickerItemDTO) int64 {
	if item.Size == nil {
		return -1
	}
	return *item.Size
}

func pickerTimeValue(item filePickerItemDTO) time.Time {
	value, err := time.Parse(time.RFC3339, item.ModifiedAt)
	if err != nil {
		return time.Time{}
	}
	return value
}

func normalizePickerSort(value string) string {
	switch strings.TrimSpace(value) {
	case "type", "size", "modifiedAt":
		return strings.TrimSpace(value)
	default:
		return "name"
	}
}

func normalizePickerOrder(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "desc") {
		return "desc"
	}
	return "asc"
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
	selectable := readable && (!symlink || resolved.root.FollowSymlinks) && (itemType == config.ResourceFile && resolved.root.AllowSelectFiles || itemType == config.ResourceDirectory && resolved.root.AllowSelectDirs)
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
	for _, root := range s.filePickerRootsForRuntime() {
		if root.ID == id {
			return root, true
		}
	}
	return config.FilePickerRoot{}, false
}

func (s *Server) filePickerRootsForRuntime() []config.FilePickerRoot {
	// 系统入口是管理员快速选路径的主入口；配置中的 roots 只作为常用位置快捷方式。
	roots := systemPickerRoots()
	seen := map[string]struct{}{}
	for _, root := range roots {
		seen[root.ID] = struct{}{}
	}
	for _, root := range s.cfg().FilePicker.Roots {
		if _, ok := seen[root.ID]; ok {
			continue
		}
		roots = append(roots, root)
	}
	return roots
}

func systemPickerRoots() []config.FilePickerRoot {
	if runtime.GOOS == "windows" {
		roots := make([]config.FilePickerRoot, 0, 26)
		for drive := 'C'; drive <= 'Z'; drive++ {
			path := string(drive) + `:\`
			if _, err := os.Stat(path); err == nil {
				roots = append(roots, config.FilePickerRoot{ID: "drive_" + strings.ToLower(string(drive)), Name: path, Path: path, AllowSelectFiles: true, AllowSelectDirs: true, ShowHidden: true, FollowSymlinks: true})
			}
		}
		return roots
	}
	return []config.FilePickerRoot{{ID: "system_root", Name: "系统根目录", Path: string(os.PathSeparator), AllowSelectFiles: true, AllowSelectDirs: true, ShowHidden: true, FollowSymlinks: true}}
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
	if strings.Contains(input, "\\") || strings.HasPrefix(input, "//") {
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
