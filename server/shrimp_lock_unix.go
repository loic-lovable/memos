//go:build darwin || linux

package server

import (
	"os"
	"syscall"

	"github.com/pkg/errors"
)

func lockShrimp(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another SHRIMP process owns this data directory")
	}
	return func() { file.Close() }, nil
}
