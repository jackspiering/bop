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
