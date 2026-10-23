//go:build !(linux || darwin || freebsd)

package storage

func diskSpace(string) (free, size int64, err error) { return 0, 0, errNoDiskSpace }
