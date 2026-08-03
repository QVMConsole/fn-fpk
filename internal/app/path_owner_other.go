//go:build !linux

package app

import "os"

func fileOwnerUID(_ os.FileInfo) (uint32, bool) {
	return 0, false
}

func fileOwnerGID(_ os.FileInfo) (uint32, bool) {
	return 0, false
}
