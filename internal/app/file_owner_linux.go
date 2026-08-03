//go:build linux

package app

import (
	"os"
	"syscall"
)

func preserveFileOwner(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Chown(path, int(stat.Uid), int(stat.Gid))
}
