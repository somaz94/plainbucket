//go:build !unix

package storage

import "os"

// inode returns 0: the platform gives no inode number.
func inode(os.FileInfo) uint64 { return 0 }
