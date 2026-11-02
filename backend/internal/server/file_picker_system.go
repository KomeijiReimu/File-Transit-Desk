package server

import (
	"os"
	"path/filepath"
	"strings"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/fsutil"
	"github.com/gofiber/fiber/v2"
)

func systemRoot(id, path string) config.FilePickerRoot {
	return config.FilePickerRoot{ID: id, Name: path, Path: path, AllowSelectFiles: true, AllowSelectDirs: true, ShowHidden: true, FollowSymlinks: true}
}

func (s *Server) filePickerResolve(c *fiber.Ctx) error {
	var in struct {
		Path string `json:"path"`
	}
	if err := c.BodyParser(&in); err != nil {
		return fiber.ErrBadRequest
	}
	in.Path = strings.TrimSpace(in.Path)
	if !filepath.IsAbs(in.Path) || strings.ContainsRune(in.Path, 0) {
		return fiber.NewError(400, "请输入服务端的绝对目录地址。")
	}
	full, err := fsutil.Canonical(in.Path)
	if err != nil {
		return friendlyPathError(err, "目录不存在或不可访问。")
	}
	info, err := os.Stat(full)
	if err != nil {
		return friendlyPathError(err, "目录不存在或不可访问。")
	}
	if !info.IsDir() {
		return fiber.NewError(400, "地址必须指向目录。")
	}
	var root config.FilePickerRoot
	if s.cfg().FilePicker.Mode == "restricted" {
		found := false
		for _, candidate := range s.cfg().FilePicker.Roots {
			inside, err := fsutil.IsInside(candidate.Path, full)
			if err == nil && inside {
				root = candidate
				found = true
				break
			}
		}
		if !found {
			return newCodedAPIError(403, "resource_path_outside_allowlist", "目录不在文件选择器允许范围内。")
		}
	} else {
		root, err = systemRootForAddress(full)
		if err != nil {
			return err
		}
	}
	rootReal, err := fsutil.Canonical(root.Path)
	if err != nil {
		return friendlyPathError(err, "文件选择器根目录不存在或不可访问。")
	}
	rel, err := filepath.Rel(rootReal, full)
	if err != nil {
		return fiber.NewError(400, "无法解析目录地址。")
	}
	if rel == "." {
		rel = ""
	}
	virtual := "/" + filepath.ToSlash(rel)
	if _, err := s.resolvePickerPath(root.ID, virtual); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"root": filePickerRootDTO{ID: root.ID, Name: root.Name, Path: root.Path, AllowSelectFiles: root.AllowSelectFiles, AllowSelectDirs: root.AllowSelectDirs}, "path": virtual})
}
