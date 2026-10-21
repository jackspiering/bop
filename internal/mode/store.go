package mode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Store persists the sticky mode in a single file inside Dir.
type Store struct{ Dir string }

func (s Store) path() string { return filepath.Join(s.Dir, "mode") }

// Load returns the sticky mode, if one is stored.
func (s Store) Load() (Mode, bool, error) {
	b, err := os.ReadFile(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return Mode{}, false, nil
	}
	// bop clear cannot help here: serve exits before the control listener
	// binds, and distroless has no shell to delete the file with.
	remedy := fmt.Sprintf("bop cannot start while it exists: recreate the container "+
		"(docker rm, compose up --force-recreate), or delete the file if %s is a mounted volume", s.Dir)
	if err != nil {
		return Mode{}, false, fmt.Errorf("cannot read sticky mode file %s: %w; %s", s.path(), err, remedy)
	}
	m, err := Parse(strings.TrimSpace(string(b)))
	if err != nil {
		return Mode{}, false, fmt.Errorf("sticky mode file %s is invalid: %w; %s", s.path(), err, remedy)
	}
	m.Sticky = true
	return m, true, nil
}

// Save writes m (forced sticky) atomically.
func (s Store) Save(m Mode) error {
	m.Sticky = true
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return explainSaveError(s.Dir, err)
	}
	tmp, err := os.CreateTemp(s.Dir, ".mode-*")
	if err != nil {
		return explainSaveError(s.Dir, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(m.String() + "\n"); err != nil {
		tmp.Close()
		return explainSaveError(s.Dir, err)
	}
	if err := tmp.Close(); err != nil {
		return explainSaveError(s.Dir, err)
	}
	if err := os.Rename(tmp.Name(), s.path()); err != nil {
		return explainSaveError(s.Dir, err)
	}
	return nil
}

// explainSaveError wraps a Save failure and, for the two causes a user can
// act on, says what to change. errors.Is still sees the original errno.
func explainSaveError(dir string, err error) error {
	wrapped := fmt.Errorf("cannot persist sticky mode: %w", err)
	switch {
	case errors.Is(err, syscall.EROFS):
		return errors.Join(wrapped, fmt.Errorf("sticky=true needs a writable %s: it cannot work with a read-only root filesystem", dir))
	case errors.Is(err, fs.ErrPermission):
		return errors.Join(wrapped, fmt.Errorf("sticky=true needs %s to be writable by this user (uid %d); set BOP_STATE_DIR to a directory it can write", dir, os.Getuid()))
	}
	return wrapped
}

// removeFile is os.Remove, replaceable in tests.
var removeFile = os.Remove

// Clear removes the sticky file. A missing file is not an error.
func (s Store) Clear() error {
	p := s.path()
	err := removeFile(p)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	// On a read-only mount Linux unlink(2)/rmdir(2) return EROFS before
	// looking the name up, even when it does not exist.
	if _, lerr := os.Lstat(p); errors.Is(lerr, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("cannot remove sticky mode %s: %w", p, err)
}
