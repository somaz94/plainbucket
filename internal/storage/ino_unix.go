//go:build unix

package storage

import (
	"os"
	"syscall"
)

// inode returns fi's inode number, or 0 when it is not known.
func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}
