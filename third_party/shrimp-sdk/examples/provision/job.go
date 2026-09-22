package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// This example supports one trusted local user. It is not a multi-user job
// service. The private parent must already exist and remain under that user's control.
type job struct{ root *os.Root }

func openJob(path string, create bool) (*job, error) {
	path = filepath.Clean(path)
	parent, name := filepath.Dir(path), filepath.Base(path)
	if name == "." || name == string(filepath.Separator) {
		return nil, errors.New("job must name a child directory")
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, err
	}
	if !privateDirectory(info) {
		return nil, errors.New("job parent must be an owned private directory (0700), not a symlink")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if create {
		if err := root.Mkdir(name, 0700); err != nil {
			return nil, fmt.Errorf("create fresh job; existing jobs require recover or retry: %w", err)
		}
		if err := syncDirectory(root); err != nil {
			return nil, err
		}
	}
	info, err = root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !privateDirectory(info) {
		return nil, errors.New("job must be an owned private directory (0700), not a symlink")
	}
	directory, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return &job{root: directory}, nil
}

func privateDirectory(info os.FileInfo) bool {
	return info.IsDir() && info.Mode().Perm()&0077 == 0 && ownedFile(info)
}

func syncDirectory(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (j *job) close() { _ = j.root.Close() }

func (j *job) load() ([]byte, error) {
	f, err := openPrivateFile(j.root, "intent.json", os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("load saved intent; missing or incomplete jobs cannot be resumed: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 262145))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > 262144 {
		return nil, errors.New("saved intent is empty or oversized")
	}
	return raw, nil
}

// save never truncates, replaces or silently completes a partial intent. Retrying
// identical bytes re-syncs the file and directory after an earlier uncertain sync.
func (j *job) save(ctx context.Context, _ string, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := openPrivateFile(j.root, "intent.json", os.O_RDWR|os.O_CREATE|os.O_EXCL)
	if errors.Is(err, os.ErrExist) {
		f, err = openPrivateFile(j.root, "intent.json", os.O_RDWR)
		if err != nil {
			return err
		}
		defer f.Close()
		existing, err := io.ReadAll(io.LimitReader(f, 262145))
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, raw) {
			return errors.New("saved intent differs; refusing to replace it")
		}
	} else if err != nil {
		return err
	} else {
		defer f.Close()
		if n, err := f.Write(raw); err != nil {
			return err
		} else if n != len(raw) {
			return io.ErrShortWrite
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDirectory(j.root)
}
