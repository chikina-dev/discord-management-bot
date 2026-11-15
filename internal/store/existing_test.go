package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenExistingNeverInitializesMissingOrEmptyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "gateway.db")
	if s, err := OpenExisting(path); err == nil {
		s.Close()
		t.Fatal("missing source was opened")
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source directory was created: %v", err)
	}
	path = filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenExisting(path); err == nil {
		s.Close()
		t.Fatal("empty source was initialized")
	}
	info, _ := os.Stat(path)
	if info.Size() != 0 {
		t.Fatal("empty source was modified")
	}
}

func TestOpenExistingAndBoundedNativeSnapshot(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	existing, err := OpenExisting(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	size, err := existing.SnapshotSize(ctx)
	if err != nil || size <= 0 {
		t.Fatalf("logical size: %d, %v", size, err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := existing.BackupLimited(ctx, path, size-1); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("capacity was not enforced: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected snapshot left an output")
	}
	if err := existing.BackupLimited(ctx, path, size); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	restored.Close()
	before, _ := os.ReadFile(path)
	if err := existing.BackupLimited(ctx, path, size); err == nil {
		t.Fatal("snapshot overwrote an existing file")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("existing snapshot changed")
	}
}
