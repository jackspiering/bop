//go:build unix

package mode

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// OnOwnMount reports whether s.Dir is on a different filesystem than "/",
// such as a tmpfs or a volume. A sticky mode then lasts exactly as long as
// that mount, not as long as the container's writable layer.
func (s Store) OnOwnMount() (bool, error) { return onOwnMount(s.Dir, "/") }

func onOwnMount(dir, root string) (bool, error) {
	d, err := deviceOf(dir)
	if err != nil {
		return false, err
	}
	r, err := deviceOf(root)
	if err != nil {
		return false, err
	}
	return d != r, nil
}

// deviceOf returns the device of path or, if it does not exist yet, of its
// nearest existing parent (where Save would create it).
func deviceOf(path string) (uint64, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	for {
		fi, err := os.Stat(p)
		if err == nil {
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, errors.New("no device information for " + p)
			}
			return uint64(st.Dev), nil // Dev is narrower than uint64 on some platforms
		}
		if !errors.Is(err, fs.ErrNotExist) || p == filepath.Dir(p) {
			return 0, err
		}
		p = filepath.Dir(p)
	}
}
