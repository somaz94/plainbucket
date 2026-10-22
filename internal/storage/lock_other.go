//go:build !unix

package storage

import "errors"

// Lock is not supported here; the caller must keep a root to one process itself.
func (s *Store) Lock() error { return errors.ErrUnsupported }
