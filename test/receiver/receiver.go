package receiver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	_ "github.com/mattn/go-sqlite3"
)

func New(t *testing.T, key ed25519.PrivateKey, upstream ed25519.PublicKey, callback, destination string) http.Handler {
	t.Helper()
	callbackURL, err := url.Parse(callback)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "ledger.db")+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=FULL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("CREATE TABLE jobs(id TEXT PRIMARY KEY,hash TEXT NOT NULL,envelope BLOB NOT NULL,accepted_at TEXT NOT NULL,result BLOB,reported INTEGER NOT NULL DEFAULT 0)"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mux := http.NewServeMux()
	addGatewayFixture(mux, ctx)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("POST /events", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(413)
			return
		}
		if err = protocol.Verify(upstream, r.Method, protocol.RequestURL(r), b, r.Header, time.Now()); err != nil {
			w.WriteHeader(401)
			return
		}
		var envelope protocol.Envelope
		if err = json.Unmarshal(b, &envelope); err != nil || envelope.DestinationID != destination || envelope.DeliveryID == "" {
			w.WriteHeader(400)
			return
		}
		accepted := protocol.Time(time.Now())
		hash := protocol.Digest(b)
		_, err = db.ExecContext(r.Context(), "INSERT INTO jobs(id,hash,envelope,accepted_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING", envelope.DeliveryID, hash, b, accepted)
		if err != nil {
			w.WriteHeader(503)
			return
		}
		var oldHash string
		if err = db.QueryRowContext(r.Context(), "SELECT hash,accepted_at FROM jobs WHERE id=?", envelope.DeliveryID).Scan(&oldHash, &accepted); err != nil {
			w.WriteHeader(503)
			return
		}
		if oldHash != hash {
			w.WriteHeader(409)
			return
		}
		body, _ := protocol.JSON(protocol.Acceptance{DeliveryID: envelope.DeliveryID, Timestamps: protocol.Timestamps{WorkerAcceptedAt: accepted}})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		w.Write(body)
	})
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		for ctx.Err() == nil {
			var id, accepted string
			var requestBody, resultBody []byte
			err := db.QueryRowContext(ctx, "SELECT id,envelope,accepted_at,result FROM jobs WHERE reported=0 ORDER BY rowid LIMIT 1").Scan(&id, &requestBody, &accepted, &resultBody)
			if errors.Is(err, sql.ErrNoRows) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
				continue
			}
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				continue
			}
			if len(resultBody) == 0 {
				var envelope protocol.Envelope
				json.Unmarshal(requestBody, &envelope)
				var data struct {
					Status protocol.ResultStatus `json:"test_status"`
				}
				json.Unmarshal(envelope.Data, &data)
				status := data.Status
				switch status {
				case protocol.Success, protocol.PartialSuccess, protocol.Failure, protocol.Skipped:
				default:
					status = protocol.Success
				}
				selected := 0
				if envelope.Attachments != nil {
					selected = len(*envelope.Attachments)
				}
				result := protocol.Result{DeliveryID: id, DestinationID: envelope.DestinationID, Status: status, Timestamps: protocol.Timestamps{WorkerAcceptedAt: accepted, WorkerFinishedAt: protocol.Time(time.Now())}}
				result.Details, _ = protocol.JSON(map[string]any{"fixture": true, "selected_attachments": selected})
				resultBody, _ = protocol.JSON(result)
				if _, err = db.ExecContext(ctx, "UPDATE jobs SET result=? WHERE id=?", resultBody, id); err != nil {
					continue
				}
			}
			req, _ := http.NewRequestWithContext(ctx, "POST", callbackURL.String(), bytes.NewReader(resultBody))
			req.Header = protocol.Sign(key, "POST", callbackURL, resultBody, time.Now())
			resp, err := client.Do(req)
			if err == nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					_, _ = db.ExecContext(ctx, "UPDATE jobs SET reported=1 WHERE id=?", id)
					continue
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-workerDone
		db.Close()
	})
	return mux
}
