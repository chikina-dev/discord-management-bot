package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/chikina-dev/discord-management-bot/internal/maintenance"
	"github.com/chikina-dev/discord-management-bot/internal/store"
)

type objectFixture struct {
	mu                sync.Mutex
	parts             [][]byte
	complete, aborted int
	failure           string
	started           chan struct{}
}

func testS3(t *testing.T, failure string) (*s3.Client, *objectFixture) {
	t.Helper()
	f := &objectFixture{failure: failure, started: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Query().Has("uploads"):
			if r.Header.Get("x-amz-checksum-algorithm") != "CRC32" || r.Header.Get("x-amz-checksum-type") != "COMPOSITE" {
				t.Error("multipart initiation did not select CRC32 composite checksums")
			}
			io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>fixture</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "PUT":
			if r.Header.Get("x-amz-sdk-checksum-algorithm") != "CRC32" {
				t.Error("part checksum algorithm differs from multipart initiation")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upload: %v", err)
			}
			if f.failure == "cancel" {
				close(f.started)
				<-r.Context().Done()
				return
			}
			if f.failure == "part" {
				w.WriteHeader(503)
				io.WriteString(w, `<Error><Code>SlowDown</Code><Message>fixture failure</Message></Error>`)
				return
			}
			f.parts = append(f.parts, body)
			w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, len(f.parts)))
			sum := make([]byte, 4)
			binary.BigEndian.PutUint32(sum, crc32.ChecksumIEEE(body))
			w.Header().Set("x-amz-checksum-crc32", base64.StdEncoding.EncodeToString(sum))
		case r.Method == "POST":
			if r.Header.Get("x-amz-checksum-type") != "COMPOSITE" {
				t.Error("completion checksum type differs from multipart initiation")
			}
			if f.failure == "complete" {
				w.WriteHeader(503)
				io.WriteString(w, `<Error><Code>SlowDown</Code><Message>fixture failure</Message></Error>`)
				return
			}
			var completed struct {
				Parts []struct {
					Checksum string `xml:"ChecksumCRC32"`
				} `xml:"Part"`
			}
			if err := xml.NewDecoder(r.Body).Decode(&completed); err != nil {
				t.Errorf("complete XML: %v", err)
			}
			if len(completed.Parts) != len(f.parts) {
				t.Errorf("completed wrong parts: %d", len(completed.Parts))
			}
			for i, part := range completed.Parts {
				sum := make([]byte, 4)
				binary.BigEndian.PutUint32(sum, crc32.ChecksumIEEE(f.parts[i]))
				if part.Checksum != base64.StdEncoding.EncodeToString(sum) {
					t.Errorf("part checksum lost: %q", part.Checksum)
				}
			}
			f.complete++
			io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"fixture"</ETag></CompleteMultipartUploadResult>`)
		case r.Method == "DELETE":
			f.aborted++
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected S3 request: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	config := aws.Config{Region: "ap-northeast-1", HTTPClient: server.Client(), RetryMaxAttempts: 1, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "fixture", SecretAccessKey: "fixture"}, nil
		})}
	return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(server.URL); o.UsePathStyle = true }), f
}
func testDatabase(t *testing.T) (string, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for key, value := range map[string]string{"retained": "fixture-data", "last_backup_success": "2000-01-01T00:00:00.000Z"} {
		if err := db.SetMetadata(context.Background(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	return path, db
}
func assertScratchClean(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "backup-tmp", "snapshot-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("scratch not cleaned: %v %v", matches, err)
	}
}

func TestBackupExistingSnapshotAndSuccessMarker(t *testing.T) {
	ctx := context.Background()
	path, db := testDatabase(t)
	client, object := testS3(t, "")
	gateway, err := maintenance.AcquireShared(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	stale := filepath.Join(filepath.Dir(path), "backup-tmp", "snapshot-interrupted")
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "gateway.db"), []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := Run(ctx, path, "fixture-backups", client)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" || object.complete != 1 || object.aborted != 0 {
		t.Fatalf("upload did not complete: %q %+v", key, object)
	}
	marker, err := db.Metadata(ctx, "last_backup_success")
	if err != nil || marker == "2000-01-01T00:00:00.000Z" {
		t.Fatalf("success not recorded: %q %v", marker, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, marker); err != nil {
		t.Fatal(err)
	}
	archive, err := gzip.NewReader(bytes.NewReader(bytes.Join(object.parts, nil)))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := io.ReadAll(archive)
	archive.Close()
	if err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(restoredPath, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := store.OpenExisting(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if value, err := restored.Metadata(ctx, "retained"); err != nil || value != "fixture-data" {
		t.Fatalf("snapshot lost data: %q %v", value, err)
	}
	if value, err := restored.Metadata(ctx, "last_backup_success"); err != nil || value != "2000-01-01T00:00:00.000Z" {
		t.Fatalf("snapshot/commit ordering changed: %q %v", value, err)
	}
	assertScratchClean(t, path)
}

func TestUploadFailureDoesNotRecordSuccess(t *testing.T) {
	for _, failure := range []string{"part", "complete"} {
		t.Run(failure, func(t *testing.T) {
			path, db := testDatabase(t)
			client, object := testS3(t, failure)
			if _, err := Run(context.Background(), path, "fixture-backups", client); err == nil {
				t.Fatal("failed upload accepted")
			}
			if object.complete != 0 || object.aborted != 1 {
				t.Fatalf("failed upload not aborted: %+v", object)
			}
			if marker, err := db.Metadata(context.Background(), "last_backup_success"); err != nil || marker != "2000-01-01T00:00:00.000Z" {
				t.Fatalf("failure marked successful: %q %v", marker, err)
			}
			assertScratchClean(t, path)
		})
	}
}

func TestMissingSourceIsNotCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := Run(context.Background(), path, "fixture", nil); err == nil {
		t.Fatal("missing source accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup created missing source: %v", err)
	}
}

func TestMaintenanceAndActiveScratchExcludeBackup(t *testing.T) {
	path, _ := testDatabase(t)
	parent := filepath.Dir(path)
	replacement, err := maintenance.AcquireExclusive(parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), path, "fixture", nil); !errors.Is(err, maintenance.ErrBusy) {
		t.Fatalf("backup entered replacement: %v", err)
	}
	replacement.Close()
	scratch := filepath.Join(parent, "backup-tmp")
	active := filepath.Join(scratch, "snapshot-active")
	if err := os.MkdirAll(active, 0700); err != nil {
		t.Fatal(err)
	}
	otherBackup, err := maintenance.AcquireExclusive(scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer otherBackup.Close()
	if _, err := Run(context.Background(), path, "fixture", nil); !errors.Is(err, maintenance.ErrBusy) {
		t.Fatalf("concurrent backup entered scratch: %v", err)
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("active snapshot was removed: %v", err)
	}
}

func TestSnapshotCapacityRefusal(t *testing.T) {
	if _, err := snapshotBudget(100, reserveBytes+99); !errors.Is(err, ErrCapacity) {
		t.Fatalf("insufficient space accepted: %v", err)
	}
	if limit, err := snapshotBudget(100, reserveBytes+100); err != nil || limit != 100 {
		t.Fatalf("safe boundary rejected: %d %v", limit, err)
	}
	if _, err := snapshotBudget(0, reserveBytes+100); !errors.Is(err, ErrCapacity) {
		t.Fatal("empty source accepted")
	}
}

func TestCompressionUsesBoundedMultipartBodies(t *testing.T) {
	client, object := testS3(t, "")
	raw := make([]byte, partBytes+32768)
	rand.New(rand.NewSource(42)).Read(raw)
	if err := upload(context.Background(), client, "fixture", "sqlite/test.db.gz", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if len(object.parts) < 2 || len(object.parts[0]) != partBytes {
		t.Fatalf("wrong multipart partition: %d", len(object.parts))
	}
	for _, part := range object.parts {
		if len(part) > partBytes {
			t.Fatalf("part exceeds bound: %d", len(part))
		}
	}
	reader, err := gzip.NewReader(bytes.NewReader(bytes.Join(object.parts, nil)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(raw, got) {
		t.Fatalf("compressed stream corrupted: %v", err)
	}
}

func TestCancellationAbortsBeforeReleasingMaintenance(t *testing.T) {
	path, db := testDatabase(t)
	client, object := testS3(t, "cancel")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, path, "fixture", client); done <- err }()
	select {
	case <-object.started:
	case <-time.After(5 * time.Second):
		t.Fatal("backup did not reach upload")
	}
	if lease, err := maintenance.AcquireExclusive(filepath.Dir(path)); !errors.Is(err, maintenance.ErrBusy) {
		if lease != nil {
			lease.Close()
		}
		t.Fatalf("replacement entered active upload: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled upload did not finish")
	}
	replacement, err := maintenance.AcquireExclusive(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	object.mu.Lock()
	aborted := object.aborted
	object.mu.Unlock()
	if aborted != 1 {
		t.Fatalf("multipart upload left behind: %d", aborted)
	}
	if marker, err := db.Metadata(context.Background(), "last_backup_success"); err != nil || marker != "2000-01-01T00:00:00.000Z" {
		t.Fatalf("canceled upload marked success: %q %v", marker, err)
	}
	assertScratchClean(t, path)
}
