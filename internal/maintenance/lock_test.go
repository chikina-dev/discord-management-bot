package maintenance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSharedWritersExcludeReplacement(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireShared(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireShared(dir)
	if err != nil {
		t.Fatal(err)
	}
	if lease, err := AcquireExclusive(dir); !errors.Is(err, ErrBusy) {
		if lease != nil {
			lease.Close()
		}
		t.Fatalf("replacement entered while writers active: %v", err)
	}
	first.Close()
	if lease, err := AcquireExclusive(dir); !errors.Is(err, ErrBusy) {
		if lease != nil {
			lease.Close()
		}
		t.Fatalf("replacement entered while backup active: %v", err)
	}
	second.Close()
	before, err := os.Stat(filepath.Join(dir, ".maintenance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := AcquireExclusive(dir)
	if err != nil {
		t.Fatal(err)
	}
	if lease, err := AcquireShared(dir); !errors.Is(err, ErrBusy) {
		if lease != nil {
			lease.Close()
		}
		t.Fatalf("writer entered during replacement: %v", err)
	}
	replacement.Close()
	writer, err := AcquireShared(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	after, err := os.Stat(filepath.Join(dir, ".maintenance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Mode().Perm() != 0600 {
		t.Fatal("maintenance must retain the same private lock inode")
	}
}
