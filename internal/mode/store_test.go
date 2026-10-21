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
	if _, _, err := (Store{Dir: dir}).Load(); err == nil || !strings.Contains(err.Error(), "bop clear") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestStoreSaveFailureExplainsReadOnly(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A path below a regular file cannot be created, like a read-only layer.
	err := Store{Dir: filepath.Join(blocker, "bop")}.Save(Mode{Name: Hang})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("Save error = %v, want read-only hint", err)
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
