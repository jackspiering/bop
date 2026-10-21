package mode

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStore(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "state")}

	if _, ok, err := s.Load(); ok || err != nil {
		t.Fatalf("empty Load = %v, %v", ok, err)
	}
	want, _ := Parse("crash exit=3 after=2s")
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load()
	if err != nil || !ok {
		t.Fatalf("Load = %v, %v", ok, err)
	}
	want.Sticky = true
	if got != want {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Load(); ok {
		t.Fatal("Load after Clear still found a mode")
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("second Clear: %v", err)
	}
}

func TestStoreInvalidFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte("explode\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := (Store{Dir: dir}).Load()
	if err == nil || !strings.Contains(err.Error(), "recreate") || strings.Contains(err.Error(), "bop clear") {
		t.Fatalf("Load error = %v, want a recreate remedy and no bop clear", err)
	}
}

func TestExplainSaveError(t *testing.T) {
	tests := []struct {
		errno     syscall.Errno
		want, not []string
	}{
		{syscall.EROFS, []string{"/state", "read-only"}, []string{"uid"}},
		{syscall.EACCES, []string{"/state", "uid", "BOP_STATE_DIR"}, []string{"read-only"}},
		{syscall.ENOSPC, []string{"no space"}, []string{"read-only", "uid"}},
		{syscall.ENOTDIR, []string{"not a directory"}, []string{"read-only", "uid"}},
	}
	for _, tt := range tests {
		t.Run(tt.errno.Error(), func(t *testing.T) {
			err := explainSaveError("/state", &fs.PathError{Op: "open", Path: "/state/.mode-1", Err: tt.errno})
			if !errors.Is(err, tt.errno) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tt.errno)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q: %v", w, err)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(err.Error(), n) {
					t.Errorf("error should not mention %q: %v", n, err)
				}
			}
		})
	}
}

// stubRemove makes removeFile fail with EROFS, as Linux does on a read-only
// mount whether or not the name exists.
func stubRemove(t *testing.T) {
	t.Helper()
	orig := removeFile
	removeFile = func(p string) error { return &fs.PathError{Op: "remove", Path: p, Err: syscall.EROFS} }
	t.Cleanup(func() { removeFile = orig })
}

func TestStoreClearToleratesEROFSForMissingFile(t *testing.T) {
	stubRemove(t)
	if err := (Store{Dir: t.TempDir()}).Clear(); err != nil {
		t.Fatalf("Clear = %v, want nil: there is nothing to remove", err)
	}
}

func TestStoreClearReportsEROFSForExistingFile(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Save(Mode{Name: Hang}); err != nil {
		t.Fatal(err)
	}
	stubRemove(t)
	if err := s.Clear(); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("Clear = %v, want EROFS", err)
	}
}

func TestStoreSavePermissionDeniedHasNoReadOnlyHint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := Store{Dir: dir}.Save(Mode{Name: Hang})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Save = %v, want a permission error", err)
	}
	if strings.Contains(err.Error(), "read-only") {
		t.Fatalf("Save error blames a read-only filesystem: %v", err)
	}
	for _, want := range []string{dir, "BOP_STATE_DIR", "uid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Save error lacks %q: %v", want, err)
		}
	}
}

// stubSync replaces syncFile and syncDir for one test.
func stubSync(t *testing.T, file func(*os.File) error, dir func(string) error) {
	t.Helper()
	origFile, origDir := syncFile, syncDir
	syncFile, syncDir = file, dir
	t.Cleanup(func() { syncFile, syncDir = origFile, origDir })
}

func TestStoreSaveSyncsBeforeAndAfterRename(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	var calls []string
	stubSync(t,
		func(f *os.File) error {
			if _, err := os.Stat(s.path()); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("mode published before the file sync: %v", err)
			}
			calls = append(calls, "file")
			return f.Sync()
		},
		func(dir string) error {
			if dir != s.Dir {
				t.Errorf("synced dir %q, want %q", dir, s.Dir)
			}
			if b, err := os.ReadFile(s.path()); err != nil || string(b) != "hang sticky=true\n" {
				t.Errorf("at dir sync, mode = %q, %v", b, err)
			}
			calls = append(calls, "dir")
			return nil
		})
	if err := s.Save(Mode{Name: Hang}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "file,dir" {
		t.Fatalf("sync calls = %v, want [file dir]", calls)
	}
}

func TestStoreSaveFileSyncError(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	stubSync(t, func(*os.File) error { return syscall.EIO }, func(string) error { return nil })
	if err := s.Save(Mode{Name: Hang}); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Save = %v, want EIO", err)
	}
	if _, ok, err := s.Load(); ok || err != nil {
		t.Fatalf("a failed sync published a mode: %v, %v", ok, err)
	}
	if left, _ := filepath.Glob(filepath.Join(s.Dir, ".mode-*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestStoreSaveDirSyncErrorIgnored(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	stubSync(t, (*os.File).Sync, func(string) error { return syscall.EINVAL })
	if err := s.Save(Mode{Name: Hang}); err != nil {
		t.Fatalf("Save = %v, want nil: the mode is already in place", err)
	}
	if m, ok, err := s.Load(); !ok || err != nil || m.Name != Hang {
		t.Fatalf("Load = %+v, %v, %v", m, ok, err)
	}
}
