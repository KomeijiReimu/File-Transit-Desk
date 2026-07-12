package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/fsutil"

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
	Path             string `json:"path"`
	AllowSelectFiles bool   `json:"allowSelectFiles"`
	AllowSelectDirs  bool   `json:"allowSelectDirs"`
}

type filePickerItemDTO struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Type          string `json:"type"`
	Size          *int64 `json:"size"`
	ModifiedAt    string `json:"modifiedAt,omitempty"`
	Hidden        bool   `json:"hidden"`
	Symlink       bool   `json:"symlink"`
	Selectable    bool   `json:"selectable"`
	Readable      bool   `json:"readable"`
	MetadataKnown bool   `json:"metadataKnown"`
	Downloadable  bool   `json:"downloadable"`
}

type filePickerListResponse struct {
	RootID         string              `json:"rootId"`
	Path           string              `json:"path"`
	ParentPath     string              `json:"parentPath"`
	Sort           string              `json:"sort"`
	Order          string              `json:"order"`
	Page           int64               `json:"page"`
	PageSize       int64               `json:"pageSize"`
	HasMore        bool                `json:"hasMore"`
	Truncated      bool                `json:"truncated"`
	TotalKnown     bool                `json:"totalKnown"`
	Total          *int64              `json:"total"`
	ScannedEntries int                 `json:"scannedEntries"`
	ScanLimit      int                 `json:"scanLimit"`
	Items          []filePickerItemDTO `json:"items"`
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
		out = append(out, filePickerRootDTO{ID: root.ID, Name: root.Name, Path: root.Path, AllowSelectFiles: root.AllowSelectFiles, AllowSelectDirs: root.AllowSelectDirs})
	}
	return c.JSON(out)
}

func (s *Server) filePickerList(c *fiber.Ctx) error {
	rootID := strings.TrimSpace(c.Query("rootId"))
	cfg := s.cfg().FilePicker
	page, err := parseListingPage(c, cfg.MaxPageSize, cfg.MaxScanEntries, false)
	if err != nil {
		return err
	}
	sortBy := normalizePickerSort(c.Query("sort", "name"))
	order := normalizePickerOrder(c.Query("order", "asc"))
	resolved, err := s.resolvePickerPath(rootID, c.Query("path"))
	if err != nil {
		s.criticalAudit("file_picker_denied", s.clientIP(c), fmt.Sprintf("根 %s 路径校验失败", rootID))
		return err
	}
	info, err := os.Stat(resolved.absolutePath)
	if err != nil {
		return friendlyPathError(err, "目录不存在或不可访问。")
	}
	if !info.IsDir() {
		return fiber.NewError(fiber.StatusBadRequest, "只能浏览目录。")
	}
	result, err := s.readPickerDir(resolved, page.Page, page.PageSize, sortBy, order, cfg.MaxScanEntries)
	if err != nil {
		return friendlyPathError(err, "目录无法读取，请检查服务端权限。")
	}
	return c.JSON(filePickerListResponse{RootID: resolved.root.ID, Path: resolved.virtualPath, ParentPath: pickerParentPath(resolved.virtualPath), Sort: sortBy, Order: order, Page: page.Page, PageSize: page.PageSize, HasMore: result.HasMore, Truncated: result.Truncated, TotalKnown: result.TotalKnown, Total: result.Total, ScannedEntries: result.ScannedEntries, ScanLimit: result.ScanLimit, Items: result.Items})
}

func (s *Server) filePickerValidate(c *fiber.Ctx) error {
	var in filePickerValidateRequest
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	resolved, err := s.resolvePickerPath(in.RootID, in.Path)
	if err != nil {
		s.criticalAudit("file_picker_denied", s.clientIP(c), fmt.Sprintf("根 %s 选择校验失败", in.RootID))
		return err
	}
	info, err := os.Stat(resolved.absolutePath)
	if err != nil {
		return friendlyPathError(err, "路径不存在，请刷新后重试。")
	}
	pickedType := config.ResourceFile
	if info.IsDir() {
		pickedType = config.ResourceDirectory
	} else if !info.Mode().IsRegular() {
		pickedType = "other"
	}
	if pickedType == "other" {
		return newCodedAPIError(fiber.StatusBadRequest, "resource_file_not_regular", "只能选择普通文件或目录。")
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
	if err := s.validateResourceSelection(s.cfg(), config.Dir{Type: pickedType, Path: resolved.absolutePath}); err != nil {
		return err
	}
	_ = s.store.Audit("file_picker_select", s.clientIP(c), fmt.Sprintf("%s:%s", resolved.root.ID, resolved.virtualPath))
	return c.JSON(filePickerValidateResponse{Valid: true, RootID: resolved.root.ID, Path: resolved.virtualPath, RelativePath: resolved.relativePath, Type: pickedType, AbsolutePath: resolved.absolutePath})
}

type pickerListResult struct {
	Items          []filePickerItemDTO
	HasMore        bool
	Truncated      bool
	TotalKnown     bool
	Total          *int64
	ScannedEntries int
	ScanLimit      int
}

func (s *Server) readPickerDir(resolved resolvedPickerPath, page, pageSize int64, sortBy, order string, scanLimit int) (pickerListResult, error) {
	opener := s.openDirectory
	if opener == nil {
		opener = func(path string) (fsutil.DirectoryReader, error) { return os.Open(path) }
	}
	dir, err := opener(resolved.absolutePath)
	if err != nil {
		return pickerListResult{}, err
	}
	defer dir.Close()
	entries, readErr := dir.ReadDir(scanLimit + 1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return pickerListResult{}, readErr
	}
	truncated := len(entries) > scanLimit
	if truncated {
		entries = entries[:scanLimit]
	}
	scannedEntries := len(entries)
	all := make([]filePickerItemDTO, 0, len(entries))
	for _, entry := range entries {
		item, include := s.pickerItem(resolved, entry)
		if include {
			all = append(all, item)
		}
	}
	sortPickerItems(all, sortBy, order)
	start := (page - 1) * pageSize
	if start > int64(len(all)) {
		start = int64(len(all))
	}
	end := start + pageSize
	if end < start || end > int64(len(all)) {
		end = int64(len(all))
	}
	var total *int64
	if !truncated {
		value := int64(len(all))
		total = &value
	}
	return pickerListResult{Items: append([]filePickerItemDTO(nil), all[start:end]...), HasMore: end < int64(len(all)), Truncated: truncated, TotalKnown: !truncated, Total: total, ScannedEntries: scannedEntries, ScanLimit: scanLimit}, nil
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
	if strings.HasPrefix(name, ".upload-") && strings.HasSuffix(name, ".tmp") {
		return filePickerItemDTO{}, false
	}
	hidden := strings.HasPrefix(name, ".")
	if s.pickerNameDenied(resolved.root, name, hidden) {
		return filePickerItemDTO{}, false
	}
	symlink := entry.Type()&os.ModeSymlink != 0
	entryPath := filepath.Join(resolved.absolutePath, name)
	rel := filepath.ToSlash(filepath.Join(resolved.relativePath, name))
	info, err := os.Lstat(entryPath)
	readable := err == nil
	contained := true
	itemType := "unknown"
	var size *int64
	modifiedAt := ""
	metadataKnown := false
	downloadable := false
	selectableType := "unknown"
	if err == nil {
		metadataKnown = true
		modifiedAt = info.ModTime().Format(time.RFC3339)
		if info.IsDir() {
			itemType = config.ResourceDirectory
			selectableType = config.ResourceDirectory
		} else if symlink {
			itemType = "symlink"
		} else if info.Mode().IsRegular() {
			itemType = config.ResourceFile
			selectableType = config.ResourceFile
			downloadable = true
			value := info.Size()
			size = &value
		} else {
			itemType = "other"
		}
	}
	if symlink && resolved.root.FollowSymlinks {
		if inside, insideErr := fsutil.IsInside(resolved.rootReal, entryPath); insideErr != nil || !inside {
			contained = false
		}
		if targetInfo, statErr := os.Stat(entryPath); statErr == nil {
			metadataKnown = true
			selectableType = "other"
			if targetInfo.IsDir() {
				selectableType = config.ResourceDirectory
				if contained {
					itemType = config.ResourceDirectory
				}
			} else if targetInfo.Mode().IsRegular() {
				selectableType = config.ResourceFile
				downloadable = contained
				if contained {
					itemType = config.ResourceFile
				}
				value := targetInfo.Size()
				size = &value
			}
			modifiedAt = targetInfo.ModTime().Format(time.RFC3339)
		} else {
			metadataKnown = false
			selectableType = "unknown"
			downloadable = false
		}
	}
	selectable := readable && metadataKnown && contained && (!symlink || resolved.root.FollowSymlinks) && (selectableType == config.ResourceFile && resolved.root.AllowSelectFiles || selectableType == config.ResourceDirectory && resolved.root.AllowSelectDirs)
	return filePickerItemDTO{Name: name, Path: "/" + rel, Type: itemType, Size: size, ModifiedAt: modifiedAt, Hidden: hidden, Symlink: symlink, Selectable: selectable, Readable: readable, MetadataKnown: metadataKnown, Downloadable: downloadable}, true
}

func (s *Server) resolvePickerPath(rootID, virtualPath string) (resolvedPickerPath, error) {
	root, ok := s.filePickerRoot(rootID)
	if !ok {
		return resolvedPickerPath{}, fiber.NewError(fiber.StatusNotFound, "文件选择器根目录不存在。")
	}
	rootReal, err := fsutil.Canonical(root.Path)
	if err != nil {
		return resolvedPickerPath{}, friendlyPathError(err, "文件选择器根目录不存在或不可访问。")
	}
	rel, err := cleanPickerVirtualPath(virtualPath)
	if err != nil {
		return resolvedPickerPath{}, err
	}
	target := filepath.Join(rootReal, filepath.FromSlash(rel))
	canonicalTarget, err := fsutil.Canonical(target)
	if err != nil {
		return resolvedPickerPath{}, friendlyPathError(err, "路径不存在或不可访问。")
	}
	inside, err := fsutil.IsInside(rootReal, canonicalTarget)
	if err != nil || !inside {
		return resolvedPickerPath{}, newCodedAPIError(fiber.StatusForbidden, "resource_path_outside_allowlist", "路径超出文件选择器允许范围。")
	}
	if !root.FollowSymlinks && rel != "" {
		// 文件选择器默认不跟随符号链接，避免管理员在弹窗内误入指向系统目录的链接。
		if hasSymlinkAncestor(rootReal, rel) {
			return resolvedPickerPath{}, fiber.NewError(fiber.StatusForbidden, "默认不允许进入符号链接路径。")
		}
	}
	return resolvedPickerPath{root: root, rootReal: rootReal, relativePath: rel, virtualPath: "/" + rel, absolutePath: canonicalTarget}, nil
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
	return append([]config.FilePickerRoot{}, s.cfg().FilePicker.Roots...)
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
