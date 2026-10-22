//go:build unix

package storage

import (
	"errors"
	"os"
	"syscall"
)

// Lock takes an exclusive advisory lock on the root's system directory, held until
// Close. It fails with ErrLocked while another server or fsck holds it.
func (s *Store) Lock() error {
	if s.lockFile != nil {
		return nil
	}
	f, err := os.OpenFile(s.sys("lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLocked
		}
		return err
	}
	s.lockFile = f
	return nil
}
