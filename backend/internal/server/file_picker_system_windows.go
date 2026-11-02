//go:build windows

package server

import (
	"encoding/base64"
	"path/filepath"
	"strings"

	"filetrans-backend/internal/config"
	"github.com/gofiber/fiber/v2"
	"golang.org/x/sys/windows"
)

func systemPickerRoots() ([]config.FilePickerRoot, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	out := make([]config.FilePickerRoot, 0)
	for i := 0; i < 26; i++ {
		if mask&(1<<i) != 0 {
			drive := string(rune('A' + i))
			out = append(out, systemRoot("system-drive-"+drive, drive+":\\"))
		}
	}
	return out, nil
}

func systemRootForAddress(address string) (config.FilePickerRoot, error) {
	volume := filepath.VolumeName(address)
	if len(volume) == 2 && volume[1] == ':' {
		drive := strings.ToUpper(volume[:1])
		return systemRoot("system-drive-"+drive, drive+":\\"), nil
	}
	if strings.HasPrefix(volume, `\\`) && !strings.HasPrefix(volume, `\\?\`) && !strings.HasPrefix(volume, `\\.\`) {
		parts := strings.Split(strings.TrimPrefix(volume, `\\`), `\`)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return systemRoot("system-unc-"+base64.RawURLEncoding.EncodeToString([]byte(volume)), volume+`\`), nil
		}
	}
	return config.FilePickerRoot{}, fiber.NewError(400, "目录地址须为 Windows 盘符绝对路径或 UNC 共享目录。")
}

func systemPickerRoot(id string) (config.FilePickerRoot, bool) {
	if strings.HasPrefix(id, "system-drive-") && len(id) == len("system-drive-")+1 {
		drive := strings.TrimPrefix(id, "system-drive-")
		if drive[0] >= 'A' && drive[0] <= 'Z' {
			return systemRoot(id, drive+":\\"), true
		}
	}
	if strings.HasPrefix(id, "system-unc-") {
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, "system-unc-"))
		if err == nil {
			root, err := systemRootForAddress(string(decoded) + `\`)
			if err == nil && root.ID == id {
				return root, true
			}
		}
	}
	return config.FilePickerRoot{}, false
}
