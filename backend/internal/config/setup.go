package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// InitializeExamples creates only missing first-run files. Existing settings are
// never replaced; setup exits before any database or listener is initialized.
func InitializeExamples(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	dir := filepath.Dir(path)
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return false, err
	}
	var source string
	for _, candidate := range []string{dir, filepath.Dir(exe), cwd} {
		if _, err := os.Stat(filepath.Join(candidate, "config.example.yaml")); err == nil {
			source = candidate
			break
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	if source == "" {
		return false, fmt.Errorf("找不到首次启动模板，请将 config.example.yaml 与 settings.example.yaml 放在配置目录或可执行文件旁")
	}
	bootstrap, err := os.ReadFile(filepath.Join(source, "config.example.yaml"))
	if err != nil {
		return false, err
	}
	settings, err := os.ReadFile(filepath.Join(source, "settings.example.yaml"))
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return false, err
	}
	for _, item := range []struct {
		path string
		data []byte
	}{{filepath.Join(dir, "settings.yaml"), settings}, {path, bootstrap}} {
		f, err := os.OpenFile(item.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if _, err := f.Write(item.data); err != nil {
			_ = f.Close()
			return false, fmt.Errorf("模板写入失败，请检查并修复 %s：%w", item.path, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return false, err
		}
		if err := f.Close(); err != nil {
			return false, err
		}
		if err := syncDir(dir); err != nil {
			return false, err
		}
	}
	return true, nil
}
