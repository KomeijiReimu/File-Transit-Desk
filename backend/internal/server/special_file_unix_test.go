//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
)

func TestSpecialFilesRejectedByResourceDownloadHashAndPicker(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("fifo unavailable: %v", err)
	}
	socket := filepath.Join(root, "socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	defer listener.Close()
	paths := []string{fifo, socket}
	if _, err := os.Stat("/dev/null"); err == nil {
		paths = append(paths, "/dev/null")
	}
	s := &Server{config: config.Default()}
	for _, path := range paths {
		resource := config.Dir{ID: "special", Type: config.ResourceFile, Path: path, AllowDownload: true}
		err := validateResourcePath(resource)
		var coded *codedAPIError
		if !errors.As(err, &coded) || coded.code != "resource_file_not_regular" {
			t.Fatalf("special resource %q returned %v", path, err)
		}
		if _, _, _, err := s.resolveDownloadFile(resource, ""); err == nil {
			t.Fatalf("special download target %q was accepted", path)
		}
		if _, err := fileSHA256Hex(path); !errors.Is(err, errDownloadFileNotRegular) {
			t.Fatalf("special hash target %q returned %v", path, err)
		}
		if _, err := fileResourceEntry(resource); err == nil {
			t.Fatalf("special file resource entry %q was accepted", path)
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read picker root: %v", err)
	}
	resolved := resolvedPickerPath{root: config.FilePickerRoot{AllowSelectFiles: true, AllowSelectDirs: true, FollowSymlinks: true}, rootReal: root, absolutePath: root}
	for _, entry := range entries {
		item, ok := s.pickerItem(resolved, entry)
		if !ok {
			continue
		}
		if entry.Name() == "pipe" || entry.Name() == "socket" {
			if item.Type != "other" || item.Selectable {
				t.Fatalf("special picker item %+v must be other and unselectable", item)
			}
		}
	}
}

func TestResourceAPIRejectsSpecialAndReplacementWithoutLeakingDetails(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "secret-pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("fifo unavailable: %v", err)
	}
	for _, tc := range []struct {
		name       string
		path       string
		beforeOpen func()
		wantCode   string
	}{
		{name: "initial fifo", path: fifo, wantCode: "resource_file_not_regular"},
		{name: "replacement fifo", path: filepath.Join(root, "replace.txt"), wantCode: "resource_file_not_regular", beforeOpen: func() {
			_ = os.Remove(filepath.Join(root, "replace.txt"))
			_ = syscall.Mkfifo(filepath.Join(root, "replace.txt"), 0600)
		}},
		{name: "replacement regular", path: filepath.Join(root, "replace-regular.txt"), wantCode: "resource_file_changed", beforeOpen: func() {
			replacement := filepath.Join(root, "replace-regular.tmp")
			_ = os.WriteFile(replacement, []byte("changed"), 0600)
			_ = os.Rename(replacement, filepath.Join(root, "replace-regular.txt"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.beforeOpen != nil {
				if err := os.WriteFile(tc.path, []byte("regular"), 0600); err != nil {
					t.Fatalf("write initial file: %v", err)
				}
			}
			cfg := testConfig(root)
			cfg.FilePicker.Roots = []config.FilePickerRoot{{ID: "root", Path: root, AllowSelectFiles: true, AllowSelectDirs: true}}
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := config.SaveAtomic(configPath, cfg); err != nil {
				t.Fatalf("save config: %v", err)
			}
			st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			defer st.DB.Close()
			if err := st.CreateSession("special-admin", time.Now().Add(time.Hour), "admin", ""); err != nil {
				t.Fatalf("create admin session: %v", err)
			}
			s := &Server{config: cfg, configPath: configPath, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry(), beforeResourceFileOpen: tc.beforeOpen}
			app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
			s.routes(app)
			body, _ := json.Marshal(resourceRequest{ID: "special", Name: "Special", Type: config.ResourceFile, Path: tc.path, AllowDownload: true})
			req := httptest.NewRequest(http.MethodPost, "/api/config/resources", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: "sid", Value: "special-admin"})
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("create resource: %v", err)
			}
			var payload map[string]any
			decodeJSON(t, resp, &payload)
			if payload["code"] != tc.wantCode {
				t.Fatalf("unexpected response status=%d payload=%+v", resp.StatusCode, payload)
			}
			encoded, _ := json.Marshal(payload)
			text := string(encoded)
			if strings.Contains(text, root) || strings.Contains(text, "secret-pipe") || strings.Contains(strings.ToLower(text), "fifo") || strings.Contains(strings.ToLower(text), "socket") {
				t.Fatalf("response leaked special path or type: %s", text)
			}
		})
	}
}

func TestHashReplacementWithFIFOUsesStableDownloadError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "hash.txt")
	if err := os.WriteFile(path, []byte("regular"), 0600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	fifoReplacement := filepath.Join(root, "fifo-replacement")
	if err := syscall.Mkfifo(fifoReplacement, 0600); err != nil {
		t.Skipf("fifo unavailable: %v", err)
	}
	cfg := testConfig(root)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if err := st.CreateSession("hash-special-session", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), transfers: newTransferRegistry()}
	s.duringDownloadFileHash = func() {
		if err := os.Rename(fifoReplacement, path); err != nil {
			t.Fatalf("replace with fifo: %v", err)
		}
		s.duringDownloadFileHash = nil
	}
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	s.routes(app)
	req := httptest.NewRequest(http.MethodPost, "/api/files/download-lease", strings.NewReader(`{"dirId":"default","path":"hash.txt"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "hash-special-session"})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	var payload map[string]any
	decodeJSON(t, resp, &payload)
	if resp.StatusCode != http.StatusConflict || payload["code"] != "download_file_not_regular" {
		t.Fatalf("unexpected hash special response: status=%d payload=%+v", resp.StatusCode, payload)
	}
	encoded, _ := json.Marshal(payload)
	text := strings.ToLower(string(encoded))
	if strings.Contains(text, root) || strings.Contains(text, "fifo") || strings.Contains(text, "hash.txt") {
		t.Fatalf("download error leaked path or type: %s", text)
	}
}

func TestSymlinkToRegularAllowedAndSymlinkToSpecialRejected(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular.txt")
	if err := os.WriteFile(regular, []byte("regular"), 0600); err != nil {
		t.Fatalf("write regular: %v", err)
	}
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("fifo unavailable: %v", err)
	}
	regularLink := filepath.Join(root, "regular-link")
	regularDir := filepath.Join(root, "regular-dir")
	if err := os.Mkdir(regularDir, 0755); err != nil {
		t.Fatalf("mkdir regular dir: %v", err)
	}
	directoryLink := filepath.Join(root, "directory-link")
	specialLink := filepath.Join(root, "special-link")
	if err := os.Symlink(regular, regularLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(fifo, specialLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(regularDir, directoryLink); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if err := validateResourcePath(config.Dir{ID: "regular", Type: config.ResourceFile, Path: regularLink, AllowDownload: true}); err != nil {
		t.Fatalf("symlink to regular file rejected: %v", err)
	}
	if _, err := fileSHA256Hex(regularLink); err != nil {
		t.Fatalf("hash symlink to regular file: %v", err)
	}
	err := validateResourcePath(config.Dir{ID: "special", Type: config.ResourceFile, Path: specialLink, AllowDownload: true})
	var coded *codedAPIError
	if !errors.As(err, &coded) || coded.code != "resource_file_not_regular" {
		t.Fatalf("symlink to special target returned %v", err)
	}
	if _, err := fileSHA256Hex(specialLink); !errors.Is(err, errDownloadFileNotRegular) {
		t.Fatalf("hash symlink to special target returned %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read picker root: %v", err)
	}
	s := &Server{config: config.Default()}
	resolved := resolvedPickerPath{root: config.FilePickerRoot{AllowSelectFiles: true, AllowSelectDirs: true, FollowSymlinks: true}, rootReal: root, absolutePath: root}
	for _, entry := range entries {
		if entry.Name() != "regular-link" && entry.Name() != "directory-link" && entry.Name() != "special-link" {
			continue
		}
		item, _ := s.pickerItem(resolved, entry)
		if entry.Name() == "regular-link" && (item.Type != config.ResourceFile || !item.Symlink || !item.Selectable || !item.Downloadable) {
			t.Fatalf("regular symlink picker item unexpected: %+v", item)
		}
		if entry.Name() == "special-link" && (item.Type != "symlink" || item.Selectable || item.Downloadable) {
			t.Fatalf("special symlink picker item unexpected: %+v", item)
		}
		if entry.Name() == "directory-link" && (item.Type != config.ResourceDirectory || !item.Symlink || !item.Selectable || item.Downloadable) {
			t.Fatalf("directory symlink picker item unexpected: %+v", item)
		}
	}
}
