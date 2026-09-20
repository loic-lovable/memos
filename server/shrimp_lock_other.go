//go:build !darwin && !linux

package server

import "github.com/pkg/errors"

func lockShrimp(string) (func(), error) {
	return nil, errors.New("experimental SHRIMP currently requires macOS or Linux")
}
