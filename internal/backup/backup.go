package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/chikina-dev/discord-management-bot/internal/maintenance"
	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/store"
	"syscall"
)

const reserveBytes int64 = 1 << 30
const partBytes = 5 << 20

var ErrCapacity = errors.New("insufficient space for a safe SQLite snapshot")

// Run backs up an existing database. The shared maintenance lease excludes
// replacement; the scratch lease excludes other backups and stale-file cleanup.
func Run(ctx context.Context, path, bucket string, client *s3.Client) (string, error) {
	parent := filepath.Dir(path)
	lease, err := maintenance.AcquireShared(parent)
	if err != nil {
		return "", err
	}
	defer lease.Close()
	database, err := store.OpenExisting(path)
	if err != nil {
		return "", err
	}
	defer database.Close()
	scratch := filepath.Join(parent, "backup-tmp")
	if err = os.MkdirAll(scratch, 0700); err != nil {
		return "", err
	}
	workspaceLease, err := maintenance.AcquireExclusive(scratch)
	if err != nil {
		return "", err
	}
	defer workspaceLease.Close()
	stale, err := filepath.Glob(filepath.Join(scratch, "snapshot-*"))
	if err != nil {
		return "", err
	}
	for _, path := range stale {
		if err = os.RemoveAll(path); err != nil {
			return "", err
		}
	}
	var fs syscall.Statfs_t
	if err = syscall.Statfs(scratch, &fs); err != nil {
		return "", err
	}
	size, err := database.SnapshotSize(ctx)
	if err != nil {
		return "", err
	}
	limit, err := snapshotBudget(size, int64(fs.Bavail)*int64(fs.Bsize))
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(scratch, "snapshot-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	snapshot := filepath.Join(dir, "gateway.db")
	if err = database.BackupLimited(ctx, snapshot, limit); err != nil {
		return "", err
	}
	file, err := os.Open(snapshot)
	if err != nil {
		return "", err
	}
	defer file.Close()
	key := "sqlite/" + time.Now().UTC().Format("2006/01/02/20060102T150405.000Z") + "-" + protocol.NewID("snapshot") + ".db.gz"
	if err = upload(ctx, client, bucket, key, file); err != nil {
		return "", err
	}
	if err = database.SetMetadata(ctx, "last_backup_success", protocol.Time(time.Now())); err != nil {
		return "", err
	}
	return key, nil
}

func snapshotBudget(source, available int64) (int64, error) {
	limit := available - reserveBytes
	if source <= 0 || limit < source {
		return 0, fmt.Errorf("%w: source=%d available=%d reserve=%d", ErrCapacity, source, available, reserveBytes)
	}
	return limit, nil
}

// Each multipart body is seekable for SDK signing and retries. Compression flows
// into one 5 MiB buffer, so neither a second file nor a whole-DB buffer is needed.
func upload(ctx context.Context, client *s3.Client, bucket, key string, src io.Reader) (err error) {
	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key), ContentType: aws.String("application/gzip"), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32, ChecksumType: types.ChecksumTypeComposite})
	if err != nil {
		return err
	}
	if created.UploadId == nil || *created.UploadId == "" {
		return errors.New("S3 did not return a multipart upload ID")
	}
	writer := &multipartWriter{ctx: ctx, client: client, bucket: bucket, key: key, id: *created.UploadId, buf: make([]byte, 0, partBytes)}
	complete := false
	defer func() {
		if !complete {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_, abortErr := client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId})
			if abortErr != nil {
				err = errors.Join(err, fmt.Errorf("abort multipart backup: %w", abortErr))
			}
		}
	}()
	compressed, err := gzip.NewWriterLevel(writer, gzip.BestSpeed)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(compressed, src)
	closeErr := compressed.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if err = writer.flush(); err != nil {
		return err
	}
	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId, ChecksumType: types.ChecksumTypeComposite, MultipartUpload: &types.CompletedMultipartUpload{Parts: writer.parts}})
	if err != nil {
		return err
	}
	complete = true
	return nil
}

type multipartWriter struct {
	ctx             context.Context
	client          *s3.Client
	bucket, key, id string
	buf             []byte
	parts           []types.CompletedPart
}

func (w *multipartWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(partBytes-len(w.buf), len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		written += n
		if len(w.buf) == partBytes {
			if err := w.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}
func (w *multipartWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	number := int32(len(w.parts) + 1)
	out, err := w.client.UploadPart(w.ctx, &s3.UploadPartInput{Bucket: aws.String(w.bucket), Key: aws.String(w.key), UploadId: aws.String(w.id), PartNumber: aws.Int32(number), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32, Body: bytes.NewReader(w.buf), ContentLength: aws.Int64(int64(len(w.buf)))})
	if err != nil {
		return err
	}
	if out.ETag == nil || strings.TrimSpace(*out.ETag) == "" {
		return errors.New("S3 did not return a backup part ETag")
	}
	if out.ChecksumCRC32 == nil || *out.ChecksumCRC32 == "" {
		return errors.New("S3 did not return a backup part CRC32 checksum")
	}
	w.parts = append(w.parts, types.CompletedPart{
		ETag: out.ETag, PartNumber: aws.Int32(number), ChecksumCRC32: out.ChecksumCRC32,
	})
	w.buf = w.buf[:0]
	return nil
}
