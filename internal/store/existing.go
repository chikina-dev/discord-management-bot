package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// OpenExisting opens an initialized database without creating directories or schema.
func OpenExisting(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("existing database: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, errors.New("existing database must be a nonempty regular file")
	}
	u := url.URL{Scheme: "file", Path: abs}
	db, err := sql.Open("sqlite3", u.String()+"?mode=rw&_foreign_keys=on&_busy_timeout=5000&_synchronous=FULL&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	var count int
	err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('destinations','triggers','metadata','channels','events','deliveries')`).Scan(&count)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("existing gateway schema: %w", err)
	}
	if count != 6 {
		db.Close()
		return nil, errors.New("existing gateway schema is required")
	}
	return &Store{db: db, path: abs}, nil
}

func (s *Store) SnapshotSize(ctx context.Context) (int64, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var pages, pageSize int64
	if err = conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err = conn.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, err
	}
	return pages * pageSize, nil
}
