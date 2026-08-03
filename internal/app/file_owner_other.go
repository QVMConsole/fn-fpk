//go:build !linux

package app

import "os"

func preserveFileOwner(_ string, _ os.FileInfo) error {
	return nil
}
