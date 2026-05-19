package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveBlocksTraversal(t *testing.T) {
	dir := t.TempDir()

	bad := []string{"../x", "a/../x", "..", `/etc/passwd`, `a\..\x`}
	for _, rel := range bad {
		if _, _, err := Resolve(dir, rel); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Resolve(%q) error = %v, want ErrUnsafePath", rel, err)
		}
	}
}

func TestResolveAllowsInside(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "a"), 0755); err != nil {
		t.Fatal(err)
	}
	full, rel, err := Resolve(dir, "a")
	if err != nil {
		t.Fatal(err)
	}
	if rel != "a" || full != filepath.Join(dir, "a") {
		t.Fatalf("got full=%q rel=%q", full, rel)
	}
}

func TestResolveBlocksSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	base := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(base, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, _, err := Resolve(base, "outside"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Resolve through symlink error = %v, want ErrUnsafePath", err)
	}
}

func TestResolveForCreateBlocksSymlinkParentEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	base := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(base, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, _, err := ResolveForCreate(base, "outside/new-dir"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ResolveForCreate through symlink error = %v, want ErrUnsafePath", err)
	}
}
