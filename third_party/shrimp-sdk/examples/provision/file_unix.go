//go:build darwin || linux

package main

import (
	"errors"
	"os"
	"syscall"
)

func ownedFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func openPrivateFile(root *os.Root, name string, flags int) (*os.File, error) {
	f, err := root.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ownedFile(info) {
		f.Close()
		return nil, errors.New("intent must be an owned private regular file (0600), not a symlink")
	}
	return f, nil
}
