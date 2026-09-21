//go:build darwin || linux

package main

import (
	"os"
	"syscall"

	"github.com/pkg/errors"
)

const recoveryLocksSupported = true

func lockRecovery(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another process owns this recovery installation")
	}
	return func() { file.Close() }, nil
}

func singleRecoveryFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && ok && stat.Nlink == 1
}
