//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func freshJob(t *testing.T) (*job, string) {
	t.Helper()
	path := filepath.Join(privateParent(t), "decision")
	j, err := openJob(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(j.close)
	return j, path
}

func privateParent(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJobPersistsExactIntentAcrossReopen(t *testing.T) {
	t.Parallel()
	j, path := freshJob(t)
	raw := []byte("{\"request\":\"preserve these exact bytes\"}\n")
	if err := j.save(context.Background(), "operation", raw); err != nil {
		t.Fatal(err)
	}
	j.close()
	reopened, err := openJob(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if err := reopened.save(context.Background(), "operation", raw); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	got, err := reopened.load()
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("load = %q, %v; want %q", got, err, raw)
	}
	for name, mode := range map[string]os.FileMode{path: 0700, filepath.Join(path, "intent.json"): 0600} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode = %o; want %o", name, info.Mode().Perm(), mode)
		}
	}
}

func TestJobNeverReplacesExistingBytes(t *testing.T) {
	t.Parallel()
	for _, existing := range []string{"complete original intent", "partial", ""} {
		t.Run(fmt.Sprintf("length_%d", len(existing)), func(t *testing.T) {
			t.Parallel()
			j, path := freshJob(t)
			file := filepath.Join(path, "intent.json")
			if err := os.WriteFile(file, []byte(existing), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := j.save(context.Background(), "replacement", []byte("a new intent")); err == nil {
				t.Fatal("replacement unexpectedly succeeded")
			}
			got, err := os.ReadFile(file)
			if err != nil || string(got) != existing {
				t.Fatalf("existing bytes changed: %q, %v", got, err)
			}
			after, err := os.Stat(file)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("existing file was replaced: %v", err)
			}
		})
	}
}

func TestJobConcurrentCreationHasOneWinner(t *testing.T) {
	t.Parallel()
	path := filepath.Join(privateParent(t), "decision")
	const count = 12
	start := make(chan struct{})
	results := make(chan error, count)
	for range count {
		go func() {
			<-start
			j, err := openJob(path, true)
			if err == nil {
				j.close()
			}
			results <- err
		}()
	}
	close(start)
	winners := 0
	for range count {
		if err := <-results; err == nil {
			winners++
		} else if !errors.Is(err, os.ErrExist) {
			t.Errorf("unexpected creation failure: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("successful creators = %d; want 1", winners)
	}
}

func TestJobConcurrentDistinctIntentsCannotOverwrite(t *testing.T) {
	t.Parallel()
	j, _ := freshJob(t)
	const count = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	winners := make(chan []byte, count)
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw := bytes.Repeat([]byte(fmt.Sprintf("intent-%02d;", i)), 1000)
			<-start
			if err := j.save(context.Background(), "operation", raw); err == nil {
				winners <- raw
			}
		}()
	}
	close(start)
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("successful writers = %d; want 1", len(winners))
	}
	want := <-winners
	got, err := j.load()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("persisted winner is incomplete or changed: %v", err)
	}
}

func TestJobRejectedSaveDoesNotCreateIntent(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"canceled", "closed"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			j, path := freshJob(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if reason == "canceled" {
				cancel()
			} else {
				j.close()
			}
			err := j.save(ctx, "operation", []byte("intent"))
			if err == nil || (reason == "canceled" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("save error = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(path, "intent.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected save created a file: %v", err)
			}
		})
	}
}

func TestJobRejectsUnsafeDirectories(t *testing.T) {
	t.Parallel()
	for _, unsafe := range []string{"public_parent", "symlink_parent", "public_job", "symlink_job"} {
		t.Run(unsafe, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			parent := filepath.Join(base, "parent")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "decision")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			switch unsafe {
			case "public_parent":
				if err := os.Chmod(parent, 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink_parent":
				link := filepath.Join(base, "parent-link")
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "decision")
			case "public_job":
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink_job":
				link := filepath.Join(parent, "decision-link")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			}
			if j, err := openJob(path, false); err == nil {
				j.close()
				t.Fatal("unsafe job directory was accepted")
			}
		})
	}
}

func TestJobRejectsUnsafeIntentFiles(t *testing.T) {
	t.Parallel()
	for _, unsafe := range []string{"symlink", "public", "directory", "fifo"} {
		t.Run(unsafe, func(t *testing.T) {
			t.Parallel()
			j, path := freshJob(t)
			file := filepath.Join(path, "intent.json")
			raw := []byte("private existing intent")
			var target string
			var err error
			switch unsafe {
			case "symlink":
				target = filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, raw, 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(target, file)
			case "public":
				err = os.WriteFile(file, raw, 0644)
			case "directory":
				err = os.Mkdir(file, 0700)
			case "fifo":
				err = syscall.Mkfifo(file, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := j.load(); err == nil {
				t.Fatal("unsafe intent was readable")
			}
			if err := j.save(context.Background(), "operation", raw); err == nil {
				t.Fatal("unsafe intent was accepted for retry")
			}
			if target != "" {
				got, err := os.ReadFile(target)
				if err != nil || !bytes.Equal(got, raw) {
					t.Fatalf("symlink target changed: %v", err)
				}
			}
		})
	}
}

func TestJobRejectsMissingEmptyAndOversizedIntent(t *testing.T) {
	t.Parallel()
	for _, size := range []int{-1, 0, 262145} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Parallel()
			j, path := freshJob(t)
			if size >= 0 {
				if err := os.WriteFile(filepath.Join(path, "intent.json"), bytes.Repeat([]byte("x"), size), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := j.load(); err == nil {
				t.Fatal("incomplete or oversized job was accepted")
			}
		})
	}
}
