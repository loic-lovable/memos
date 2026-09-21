//go:build !darwin && !linux

package main

import (
	"github.com/pkg/errors"
	"os"
)

const recoveryLocksSupported = false

func lockRecovery(string) (func(), error) {
	return nil, errors.New("recovery guard requires macOS or Linux")
}

func singleRecoveryFile(info os.FileInfo) bool { return info.Mode().IsRegular() }

func inspectRecoveryLock(string) (func(), error) {
	return nil, errors.New("recovery inspection requires macOS or Linux")
}
