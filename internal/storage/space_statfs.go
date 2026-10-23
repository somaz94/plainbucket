//go:build linux || darwin || freebsd

package storage

import "syscall"

func diskSpace(path string) (free, size int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize) //nolint:unconvert // width and signedness vary by GOOS and GOARCH
	return int64(st.Bavail) * bs, int64(st.Blocks) * bs, nil
}
