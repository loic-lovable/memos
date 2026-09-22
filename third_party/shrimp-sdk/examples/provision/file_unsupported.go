//go:build !darwin && !linux

package main

import (
	"errors"
	"os"
)

func ownedFile(os.FileInfo) bool { return false }

func openPrivateFile(*os.Root, string, int) (*os.File, error) {
	return nil, errors.New("this filesystem example supports Linux and macOS only")
}
