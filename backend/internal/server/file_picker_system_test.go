package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filetrans-backend/internal/store"
)

func TestAdminPickerSystemResolveAndRestrictedMode(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "MiXeD-Suffix")
	directory := filepath.Join(outside, "中文 空格")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "完整 文件.txt")
	if err := os.WriteFile(file, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(root)
	cfg.FilePicker.Mode = "system"
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer st.DB.Close()
	if err := st.CreateSession("admin", time.Now().Add(time.Hour), "admin", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession("user", time.Now().Add(time.Hour), "user", ""); err != nil {
		t.Fatal(err)
	}
	app := New(cfg, st)
	type resolveResponse struct {
		Root     filePickerRootDTO `json:"root"`
		Path     string            `json:"path"`
		FilePath string            `json:"filePath"`
	}
	resolve := func(sid, address string, want int) resolveResponse {
		t.Helper()
		payload, err := json.Marshal(map[string]string{"path": address})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/api/config/file-picker/resolve", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		var out resolveResponse
		decodeJSON(t, resp, &out)
		if resp.StatusCode != want {
			t.Fatalf("resolve status=%d want=%d", resp.StatusCode, want)
		}
		return out
	}
	resolve("user", outside, 403)
	if out := resolve("admin", outside, 200); filepath.Base(filepath.FromSlash(out.Path)) != "MiXeD-Suffix" || out.FilePath != "" {
		t.Fatalf("directory contract changed: %+v", out)
	}
	parent := resolve("admin", `"`+directory+`"`, 200)
	if filepath.Base(filepath.FromSlash(parent.Path)) != "中文 空格" || parent.FilePath != "" {
		t.Fatalf("quoted directory changed: %+v", parent)
	}
	for _, address := range []string{file, `"` + file + `"`} {
		out := resolve("admin", address, 200)
		if out.Path != parent.Path || out.FilePath != parent.Path+"/完整 文件.txt" || out.Root.ID != parent.Root.ID {
			t.Fatalf("file must locate its parent and remain pending selection: %+v", out)
		}
	}
	payload, err := json.Marshal(map[string]string{"rootId": parent.Root.ID, "path": parent.Path + "/完整 文件.txt", "expectedType": "file"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/config/file-picker/validate", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: "admin"})
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	var selection filePickerValidateResponse
	decodeJSON(t, resp, &selection)
	if resp.StatusCode != 200 || !selection.Valid || selection.Type != "file" {
		t.Fatalf("explicit file confirmation failed: status=%d body=%+v", resp.StatusCode, selection)
	}
	resolve("admin", os.DevNull, 400)
	cfg.FilePicker.DenyNames = []string{"完整 文件.txt"}
	resolve("admin", file, 403)
	cfg.FilePicker.DenyNames = nil
	cfg.FilePicker.Mode = "restricted"
	resolve("admin", outside, 403)
	resolve("admin", `"`+file+`"`, 403)
}
