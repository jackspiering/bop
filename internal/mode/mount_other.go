//go:build !unix

package mode

// OnOwnMount always reports false where device numbers are not available.
func (s Store) OnOwnMount() (bool, error) { return false, nil }
