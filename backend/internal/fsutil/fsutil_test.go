package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestResolveBlocksTraversal(t *testing.T) {
	dir := t.TempDir()

	// 同时覆盖 Unix 和 Windows 风格路径，保证跨平台都拒绝目录穿越。
	bad := []string{"../x", "a/../x", "..", `/etc/passwd`, `//server/share/file`, `\windows`, `a\..\x`, `C:\Windows`, `C:relative`, `\\server\share\file`, "safe\x00name"}
	for _, rel := range bad {
		if _, _, err := Resolve(dir, rel); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Resolve(%q) error = %v, want ErrUnsafePath", rel, err)
		}
	}
}

type fakeDirectoryReader struct {
	entries   []os.DirEntry
	requested int
}

func (r *fakeDirectoryReader) ReadDir(n int) ([]os.DirEntry, error) {
	r.requested = n
	if len(r.entries) <= n {
		return r.entries, io.EOF
	}
	return r.entries[:n], nil
}

func (r *fakeDirectoryReader) Close() error { return nil }

type fakeDirEntry struct {
	name    string
	mode    os.FileMode
	size    int64
	infoErr error
}

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return e.mode.IsDir() }
func (e fakeDirEntry) Type() os.FileMode          { return e.mode.Type() }
func (e fakeDirEntry) Info() (os.FileInfo, error) { return fakeFileInfo(e), e.infoErr }

type fakeFileInfo fakeDirEntry

func (i fakeFileInfo) Name() string       { return i.name }
func (i fakeFileInfo) Size() int64        { return i.size }
func (i fakeFileInfo) Mode() os.FileMode  { return i.mode }
func (i fakeFileInfo) ModTime() time.Time { return time.Unix(1700000000, 0) }
func (i fakeFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeFileInfo) Sys() any           { return nil }

func TestListDirectoryBoundedPaginationAndMetadataFailure(t *testing.T) {
	root := t.TempDir()
	reader := &fakeDirectoryReader{entries: []os.DirEntry{
		fakeDirEntry{name: "z.txt", mode: 0, size: 3},
		fakeDirEntry{name: "folder", mode: os.ModeDir},
		fakeDirEntry{name: ".upload-secret.tmp", mode: 0},
		fakeDirEntry{name: "unknown", mode: 0, infoErr: errors.New("transient")},
		fakeDirEntry{name: "a.txt", mode: 0, size: 1},
		fakeDirEntry{name: "sentinel.txt", mode: 0, size: 1},
	}}
	for i := len(reader.entries); i < 10000; i++ {
		reader.entries = append(reader.entries, fakeDirEntry{name: fmt.Sprintf("bulk-%05d", i), mode: 0, size: 1})
	}
	result, err := ListDirectory(root, "", ListOptions{ScanLimit: 5, Page: 1, PageSize: 2, OpenDir: func(string) (DirectoryReader, error) { return reader, nil }})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if reader.requested != 6 {
		t.Fatalf("expected exactly scanLimit+1 entries requested, got %d", reader.requested)
	}
	if !result.Truncated || result.TotalKnown || result.Total != nil || result.ScannedEntries != 5 || result.ScanLimit != 5 {
		t.Fatalf("unexpected scan metadata: %+v", result)
	}
	if len(result.Entries) != 2 || result.Entries[0].Name != "folder" || result.Entries[1].Name != "a.txt" || !result.HasMore {
		t.Fatalf("unexpected sorted page: %+v", result.Entries)
	}
	secondReader := &fakeDirectoryReader{entries: reader.entries}
	second, err := ListDirectory(root, "", ListOptions{ScanLimit: 5, Page: 2, PageSize: 2, OpenDir: func(string) (DirectoryReader, error) { return secondReader, nil }})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Entries) != 2 || second.HasMore || second.Entries[0].Type != "unknown" || second.Entries[0].MetadataKnown || second.Entries[0].Downloadable {
		t.Fatalf("unexpected final scanned page: %+v metadata=%+v", second.Entries, second)
	}
}

func TestListDirectoryPageOverflowRejectedBeforeOpen(t *testing.T) {
	opened := false
	_, err := ListDirectory(t.TempDir(), "", ListOptions{ScanLimit: 5000, Page: int64(^uint64(0) >> 1), PageSize: 200, OpenDir: func(string) (DirectoryReader, error) {
		opened = true
		return nil, errors.New("must not open")
	}})
	if !errors.Is(err, ErrPageOutOfRange) || opened {
		t.Fatalf("overflow page error=%v opened=%v", err, opened)
	}
}

func TestListDirectoryTempEntriesConsumeRawScanWindow(t *testing.T) {
	root := t.TempDir()
	entries := make([]os.DirEntry, 0, 6)
	for i := 0; i < 5; i++ {
		entries = append(entries, fakeDirEntry{name: fmt.Sprintf(".upload-%d.tmp", i), mode: 0})
	}
	entries = append(entries, fakeDirEntry{name: "visible-after-window.txt", mode: 0, size: 1})
	reader := &fakeDirectoryReader{entries: entries}
	result, err := ListDirectory(root, "", ListOptions{ScanLimit: 5, Page: 1, PageSize: 5, OpenDir: func(string) (DirectoryReader, error) { return reader, nil }})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !result.Truncated || result.TotalKnown || result.Total != nil || result.HasMore || result.ScannedEntries != 5 || len(result.Entries) != 0 {
		t.Fatalf("temp entries must consume raw scan budget without exposing probe entry: %+v", result)
	}
}

func TestListDirectoryKnownTotalAndLegacyTempFiltering(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"b.txt", "a.txt", ".upload-hidden.tmp"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	result, err := ListDirectory(root, "", ListOptions{ScanLimit: 100, Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if result.Truncated || !result.TotalKnown || result.Total == nil || *result.Total != 2 || len(result.Entries) != 2 || result.Entries[0].Name != "a.txt" {
		t.Fatalf("unexpected known-total result: %+v", result)
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

func TestListHidesUploadTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".upload-abc.tmp"), []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "done.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := ListDirectory(dir, "", ListOptions{ScanLimit: 100, Page: 1, PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Name != "done.txt" {
		t.Fatalf("expected only committed file, got %+v", result.Entries)
	}
}

func TestListDoesNotCreateMissingBase(t *testing.T) {
	// 浏览是只读操作，配置目录不存在时应返回错误，不能顺手在磁盘上创建目录。
	base := filepath.Join(t.TempDir(), "missing")
	if _, err := ListDirectory(base, "", ListOptions{ScanLimit: 100, Page: 1, PageSize: 100}); err == nil {
		t.Fatalf("expected missing base to fail")
	}
	if _, err := os.Stat(base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing base to remain absent, stat error=%v", err)
	}
}

func TestSafeNameTrimsWindowsEquivalentSuffixes(t *testing.T) {
	// 扩展名策略使用 SafeName 后的结果，尾随空格和点必须先被规范化，避免 bad.exe 这类名称绕过黑名单。
	cases := map[string]string{
		"bad.exe ":   "bad.exe",
		"bad.ps1.\t": "bad.ps1",
		"\t. ":       "file",
	}
	for input, want := range cases {
		if got := SafeName(input); got != want {
			t.Fatalf("SafeName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveBlocksSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	base := t.TempDir()
	outside := t.TempDir()
	// 目标本身是符号链接时，Resolve 必须看真实路径是否仍在 base 内。
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
	// 新文件尚不存在时也要检查最近存在父目录，防止写入符号链接指向的外部目录。
	link := filepath.Join(base, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, _, err := ResolveForCreate(base, "outside/new-dir"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ResolveForCreate through symlink error = %v, want ErrUnsafePath", err)
	}
}

func TestCanonicalAndIsInsideResolveSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(inside, "child"), 0755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	if err := os.Symlink(filepath.Join(inside, "child"), filepath.Join(root, "internal-link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	canonical, err := Canonical(filepath.Join(root, "internal-link"))
	if err != nil {
		t.Fatalf("canonical internal link: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(inside, "child"))
	if err != nil || canonical != filepath.Clean(want) {
		t.Fatalf("unexpected canonical path: got=%q want=%q err=%v", canonical, want, err)
	}
	if ok, err := IsInside(root, filepath.Join(root, "internal-link")); err != nil || !ok {
		t.Fatalf("expected internal symlink inside, ok=%v err=%v", ok, err)
	}
	if ok, err := IsInside(root, filepath.Join(root, "outside-link")); err != nil || ok {
		t.Fatalf("expected escaping symlink outside, ok=%v err=%v", ok, err)
	}
	if ok, err := IsInside(root, filepath.Join(root, "missing", "child")); err != nil || !ok {
		t.Fatalf("expected non-existing child inside, ok=%v err=%v", ok, err)
	}
}

func TestCanonicalRelativePathUsesProcessWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	target, err := os.MkdirTemp(cwd, ".canonical-relative-*")
	if err != nil {
		t.Fatalf("create cwd temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(target) })
	relative := filepath.Base(target)
	fromRelative, err := Canonical(relative)
	if err != nil {
		t.Fatalf("canonical relative: %v", err)
	}
	fromAbsolute, err := Canonical(target)
	if err != nil || fromRelative != fromAbsolute {
		t.Fatalf("relative path must use process cwd, relative=%q absolute=%q err=%v", fromRelative, fromAbsolute, err)
	}
}

func TestSamePathUsesFileIdentityAndMissingPathEquivalence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	alias := filepath.Join(dir, "alias.txt")
	if err := os.Link(file, alias); err == nil {
		equal, sameErr := SamePath(file, alias)
		if sameErr != nil || !equal {
			t.Fatalf("expected hard links to be same file, equal=%v err=%v", equal, sameErr)
		}
	}
	missing := filepath.Join(dir, "missing.db-wal")
	equivalent := filepath.Join(dir, ".", "missing.db-wal")
	equal, err := SamePath(missing, equivalent)
	if err != nil || !equal {
		t.Fatalf("expected missing equivalent paths equal, equal=%v err=%v", equal, err)
	}
}

func TestIsInsideDoesNotUseStringPrefix(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "root")
	sibling := filepath.Join(parent, "root-other")
	if err := os.Mkdir(base, 0755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	if err := os.Mkdir(sibling, 0755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	inside, err := IsInside(base, sibling)
	if err != nil || inside {
		t.Fatalf("sibling with common string prefix must be outside, inside=%v err=%v", inside, err)
	}
}
