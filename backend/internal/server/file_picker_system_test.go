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
	if err := os.Mkdir(outside, 0700); err != nil {
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
	resolve := func(sid string, want int) {
		t.Helper()
		payload, err := json.Marshal(map[string]string{"path": outside})
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
		var out struct {
			Root filePickerRootDTO `json:"root"`
			Path string            `json:"path"`
		}
		decodeJSON(t, resp, &out)
		if resp.StatusCode != want {
			t.Fatalf("resolve status=%d want=%d", resp.StatusCode, want)
		}
		if want == 200 && filepath.Base(filepath.FromSlash(out.Path)) != "MiXeD-Suffix" {
			t.Fatalf("suffix case changed: %+v", out)
		}
	}
	resolve("user", 403)
	resolve("admin", 200)
	cfg.FilePicker.Mode = "restricted"
	resolve("admin", 403)
}
