//go:build unix

package mode

import (
	"path/filepath"
	"testing"
)

func TestOnOwnMountSameFilesystem(t *testing.T) {
	dir := t.TempDir()
	// A missing state dir is judged by its nearest existing parent.
	own, err := onOwnMount(filepath.Join(dir, "not", "yet"), dir)
	if err != nil || own {
		t.Fatalf("onOwnMount = %v, %v; want false", own, err)
	}
}

func TestOnOwnMountDifferentFilesystem(t *testing.T) {
	// /proc is always its own mount on Linux; skip where it is absent.
	if _, err := deviceOf("/proc/self"); err != nil {
		t.Skip("no /proc")
	}
	own, err := onOwnMount("/proc/self", t.TempDir())
	if err != nil || !own {
		t.Fatalf("onOwnMount = %v, %v; want true", own, err)
	}
}
