package server

import (
	"os"
	"path"
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
	if len(in.Path) >= 2 && in.Path[0] == '"' && in.Path[len(in.Path)-1] == '"' {
		in.Path = in.Path[1 : len(in.Path)-1]
	}
	if !filepath.IsAbs(in.Path) || strings.ContainsRune(in.Path, 0) {
		return fiber.NewError(400, "请输入服务端的绝对目录或文件地址。")
	}
	full, err := fsutil.Canonical(in.Path)
	if err != nil {
		return friendlyPathError(err, "目录或文件不存在或不可访问。")
	}
	info, err := os.Stat(full)
	if err != nil {
		return friendlyPathError(err, "目录或文件不存在或不可访问。")
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return newCodedAPIError(400, "resource_file_not_regular", "地址只能指向普通文件或目录。")
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
			return newCodedAPIError(403, "resource_path_outside_allowlist", "路径不在文件选择器允许范围内。")
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
		return fiber.NewError(400, "无法解析目录或文件地址。")
	}
	if rel == "." {
		if !info.IsDir() {
			return fiber.NewError(400, "文件选择器根路径必须为目录。")
		}
		rel = ""
	}
	virtual := "/" + filepath.ToSlash(rel)
	if _, err := s.resolvePickerPath(root.ID, virtual); err != nil {
		return err
	}
	out := fiber.Map{"root": filePickerRootDTO{ID: root.ID, Name: root.Name, Path: root.Path, AllowSelectFiles: root.AllowSelectFiles, AllowSelectDirs: root.AllowSelectDirs}, "path": virtual}
	if !info.IsDir() {
		name := filepath.Base(full)
		if strings.HasPrefix(name, ".upload-") && strings.HasSuffix(name, ".tmp") || s.pickerNameDenied(root, name, strings.HasPrefix(name, ".")) {
			return fiber.NewError(403, "该文件被文件选择器规则隐藏，不能选择。")
		}
		out["filePath"] = virtual
		out["path"] = path.Dir(virtual)
	}
	return c.JSON(out)
}
