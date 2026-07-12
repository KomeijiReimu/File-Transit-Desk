//go:build windows

package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanRelRejectsWindowsVolumesAndUNC(t *testing.T) {
	for _, value := range []string{`C:\Windows`, `C:relative`, `\\server\share`, `\rooted`, `//server/share`} {
		if _, err := CleanRel(value); err != ErrUnsafePath {
			t.Fatalf("CleanRel(%q) error=%v, want ErrUnsafePath", value, err)
		}
	}
}

func TestWindowsPathEquivalenceAndContainment(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "MixedCase.txt")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	equal, err := SamePath(file, strings.ToUpper(file))
	if err != nil || !equal {
		t.Fatalf("expected case-insensitive same path, equal=%v err=%v", equal, err)
	}
	if inside, err := IsInside(`C:\root`, `D:\root\child`); err != nil || inside {
		t.Fatalf("different volumes must be outside, inside=%v err=%v", inside, err)
	}
	if equal, err := SamePath(`C:\missing.db-wal`, `D:\missing.db-wal`); err != nil || equal {
		t.Fatalf("different-volume missing paths must not be equal, equal=%v err=%v", equal, err)
	}
	parent := t.TempDir()
	base := filepath.Join(parent, "root")
	sibling := filepath.Join(parent, "root-other")
	if err := os.Mkdir(base, 0755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	if err := os.Mkdir(sibling, 0755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	if inside, err := IsInside(base, sibling); err != nil || inside {
		t.Fatalf("similar prefix must be outside, inside=%v err=%v", inside, err)
	}
}
