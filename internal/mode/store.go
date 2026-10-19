package mode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	if err != nil {
		return Mode{}, false, fmt.Errorf("read sticky mode %s: %w", s.path(), err)
	}
	m, err := Parse(strings.TrimSpace(string(b)))
	if err != nil {
		return Mode{}, false, fmt.Errorf("sticky mode file %s is invalid (remove it or run `bop clear`): %w", s.path(), err)
	}
	m.Sticky = true
	return m, true, nil
}

// Save writes m (forced sticky) atomically.
func (s Store) Save(m Mode) error {
	m.Sticky = true
	hint := fmt.Errorf("sticky=true needs a writable %s: it cannot work with a read-only root filesystem", s.Dir)
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return errors.Join(fmt.Errorf("cannot persist sticky mode: %w", err), hint)
	}
	tmp, err := os.CreateTemp(s.Dir, ".mode-*")
	if err != nil {
		return errors.Join(fmt.Errorf("cannot persist sticky mode: %w", err), hint)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(m.String() + "\n"); err != nil {
		tmp.Close()
		return errors.Join(fmt.Errorf("cannot persist sticky mode: %w", err), hint)
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(fmt.Errorf("cannot persist sticky mode: %w", err), hint)
	}
	if err := os.Rename(tmp.Name(), s.path()); err != nil {
		return errors.Join(fmt.Errorf("cannot persist sticky mode: %w", err), hint)
	}
	return nil
}

// Clear removes the sticky file. A missing file is not an error.
func (s Store) Clear() error {
	if err := os.Remove(s.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot remove sticky mode %s: %w", s.path(), err)
	}
	return nil
}
