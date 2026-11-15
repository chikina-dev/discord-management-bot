package store

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schema string

var ErrNotFound = sql.ErrNoRows
var ErrConflict = errors.New("conflicting existing result")
var ErrNotSent = errors.New("delivery has not been sent")
var ErrResultOwner = errors.New("destination or signing key does not own delivery")

type Store struct {
	db   *sql.DB
	path string
}
type Destination struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	URL             string `json:"url"`
	ResultPublicKey string `json:"result_public_key"`
	KeyID           string `json:"result_key_id"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}
type Trigger struct {
	ID            string `json:"id"`
	DestinationID string `json:"destination_id"`
	Name          string `json:"name"`
	Condition     string `json:"condition"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}
type Event struct {
	ID              string          `json:"id"`
	SessionID       string          `json:"-"`
	Sequence        int64           `json:"-"`
	EventType       string          `json:"event_type"`
	Data            json.RawMessage `json:"data"`
	ReceivedAt      string          `json:"received_at"`
	SuppliedContext json.RawMessage `json:"context,omitempty"`
	TriggerIDs      []string        `json:"trigger_ids,omitempty"`
	LastError       string          `json:"last_error,omitempty"`
}
type GatewayState struct {
	SessionID      string `json:"session_id"`
	ResumeURL      string `json:"resume_url"`
	Sequence       int64  `json:"sequence"`
	Fingerprint    string `json:"fingerprint"`
	IdentifyAfter  int64  `json:"identify_after"`
	ReconnectAfter int64  `json:"reconnect_after"`
	Fatal          bool   `json:"fatal"`
}
type NewDelivery struct {
	Envelope    protocol.Envelope
	Trigger     Trigger
	Destination Destination
}
type Delivery struct {
	ID              string                 `json:"id"`
	EventID         string                 `json:"event_id"`
	TriggerID       string                 `json:"trigger_id"`
	TriggerName     string                 `json:"trigger_name"`
	Condition       string                 `json:"condition"`
	DestinationID   string                 `json:"destination_id"`
	DestinationName string                 `json:"destination_name"`
	URL             string                 `json:"url"`
	PublicKey       string                 `json:"-"`
	KeyID           string                 `json:"-"`
	EventType       string                 `json:"event_type"`
	Payload         json.RawMessage        `json:"payload,omitempty"`
	State           string                 `json:"state"`
	CreatedAt       string                 `json:"created_at"`
	Timestamps      protocol.Timestamps    `json:"timestamps"`
	StoppedAt       string                 `json:"stopped_at,omitempty"`
	StopReason      string                 `json:"stop_reason,omitempty"`
	Attempts        int                    `json:"attempts"`
	LastHTTPStatus  int                    `json:"last_http_status,omitempty"`
	LastError       string                 `json:"last_error,omitempty"`
	ResultStatus    *protocol.ResultStatus `json:"result_status"`
	Result          json.RawMessage        `json:"result,omitempty"`
}
type Stats struct {
	Pending              int     `json:"pending"`
	AwaitingResult       int     `json:"awaiting_result"`
	Finished             int     `json:"finished"`
	Stopped              int     `json:"stopped"`
	QueuedEvents         int     `json:"queued_events"`
	OldestPendingSeconds float64 `json:"oldest_pending_seconds"`
}

func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	db, err := sql.Open("sqlite3", u.String()+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_synchronous=FULL&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(abs, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: abs}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func stableID(prefix, identity string) string {
	h := sha256.Sum256([]byte(identity))
	return prefix + "_" + hex.EncodeToString(h[:16])
}
func DeliveryID(event, trigger string) string { return stableID("dl", event+"\n"+trigger) }
func EventID(session string, sequence int64) string {
	return stableID("ev", session+"\n"+strconv.FormatInt(sequence, 10))
}

func ValidateDestination(d Destination) error {
	if strings.TrimSpace(d.Name) == "" || len(d.Name) > 128 {
		return errors.New("name must contain 1–128 bytes")
	}
	u, err := url.Parse(d.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid destination URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !(u.Hostname() == "localhost" || ip != nil && (ip.IsPrivate() || ip.IsLoopback())) {
			return errors.New("destination must use HTTPS; HTTP is allowed only for private or loopback test receivers")
		}
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return errors.New("destination URL must not contain dot segments")
		}
	}
	_, err = protocol.DecodePublicKey(d.ResultPublicKey)
	return err
}
func (s *Store) PutDestination(ctx context.Context, d Destination, create bool) (Destination, error) {
	if err := ValidateDestination(d); err != nil {
		return d, err
	}
	pub, _ := protocol.DecodePublicKey(d.ResultPublicKey)
	d.KeyID = protocol.KeyID(pub)
	now := protocol.Time(time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	if create {
		if d.ID == "" {
			d.ID = protocol.NewID("dst")
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO destinations(id,name,url,result_public_key,result_key_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", d.ID, d.Name, d.URL, d.ResultPublicKey, d.KeyID, now, now)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, "UPDATE destinations SET name=?,url=?,result_public_key=?,result_key_id=?,updated_at=? WHERE id=?", d.Name, d.URL, d.ResultPublicKey, d.KeyID, now, d.ID)
		if err == nil {
			if n, _ := result.RowsAffected(); n == 0 {
				return d, ErrNotFound
			}
		}
	}
	if err != nil {
		return d, err
	}
	saved, err := scanDestination(tx.QueryRowContext(ctx, "SELECT id,name,url,result_public_key,result_key_id,created_at,updated_at FROM destinations WHERE id=?", d.ID))
	if err != nil {
		return d, err
	}
	if err = tx.Commit(); err != nil {
		return d, err
	}
	return saved, nil
}

type scanner interface{ Scan(...any) error }

func scanDestination(row scanner) (Destination, error) {
	var d Destination
	err := row.Scan(&d.ID, &d.Name, &d.URL, &d.ResultPublicKey, &d.KeyID, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}
func (s *Store) Destination(ctx context.Context, id string) (Destination, error) {
	return scanDestination(s.db.QueryRowContext(ctx, "SELECT id,name,url,result_public_key,result_key_id,created_at,updated_at FROM destinations WHERE id=?", id))
}
func (s *Store) Destinations(ctx context.Context) ([]Destination, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,name,url,result_public_key,result_key_id,created_at,updated_at FROM destinations ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Destination{}
	for rows.Next() {
		d, e := scanDestination(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) DeleteDestination(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE deliveries SET
		state=CASE WHEN state='pending' THEN 'stopped' ELSE state END,
		stopped_at=CASE WHEN state='pending' THEN ? ELSE stopped_at END,
		stop_reason=CASE WHEN state='pending' THEN 'destination_deleted' ELSE stop_reason END,
		lease_until=0 WHERE destination_id=?`, protocol.Time(time.Now()), id)
	if err != nil {
		return err
	}
	deleted, err := tx.ExecContext(ctx, "DELETE FROM destinations WHERE id=?", id)
	if err != nil {
		return err
	}
	if n, _ := deleted.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}
func scanTrigger(row scanner) (Trigger, error) {
	var r Trigger
	err := row.Scan(&r.ID, &r.DestinationID, &r.Name, &r.Condition, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}
func (s *Store) PutTrigger(ctx context.Context, r Trigger, create bool) (Trigger, error) {
	if strings.TrimSpace(r.Name) == "" || len(r.Name) > 128 || r.Condition == "" {
		return r, errors.New("name and condition are required")
	}
	now := protocol.Time(time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	if create {
		if r.ID == "" {
			r.ID = protocol.NewID("tr")
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO triggers(id,destination_id,name,condition,created_at,updated_at) VALUES(?,?,?,?,?,?)", r.ID, r.DestinationID, r.Name, r.Condition, now, now)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, "UPDATE triggers SET destination_id=?,name=?,condition=?,updated_at=? WHERE id=?", r.DestinationID, r.Name, r.Condition, now, r.ID)
		if err == nil {
			if n, _ := result.RowsAffected(); n == 0 {
				return r, ErrNotFound
			}
		}
	}
	if err != nil {
		return r, err
	}
	saved, err := scanTrigger(tx.QueryRowContext(ctx, "SELECT id,destination_id,name,condition,created_at,updated_at FROM triggers WHERE id=?", r.ID))
	if err != nil {
		return r, err
	}
	if err = tx.Commit(); err != nil {
		return r, err
	}
	return saved, nil
}
func (s *Store) Trigger(ctx context.Context, id string) (Trigger, error) {
	return scanTrigger(s.db.QueryRowContext(ctx, "SELECT id,destination_id,name,condition,created_at,updated_at FROM triggers WHERE id=?", id))
}
func (s *Store) Triggers(ctx context.Context) ([]Trigger, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,destination_id,name,condition,created_at,updated_at FROM triggers ORDER BY created_at,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Trigger{}
	for rows.Next() {
		r, e := scanTrigger(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) DeleteTrigger(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "DELETE FROM triggers WHERE id=?", id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	_, err = tx.ExecContext(ctx, "UPDATE deliveries SET state='stopped',stopped_at=?,stop_reason='trigger_deleted',lease_until=0 WHERE trigger_id=? AND state='pending'", protocol.Time(time.Now()), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Metadata(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&v)
	return v, err
}
func (s *Store) SetMetadata(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}
func (s *Store) GatewayState(ctx context.Context) (GatewayState, error) {
	raw, err := s.Metadata(ctx, "gateway")
	if errors.Is(err, ErrNotFound) {
		return GatewayState{}, nil
	}
	if err != nil {
		return GatewayState{}, err
	}
	var state GatewayState
	err = json.Unmarshal([]byte(raw), &state)
	return state, err
}
func (s *Store) SetGatewayState(ctx context.Context, state GatewayState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.SetMetadata(ctx, "gateway", string(b))
}

// SaveDispatch commits the received event and resume cursor together.
func (s *Store) SaveDispatch(ctx context.Context, state GatewayState, eventType string, data json.RawMessage, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous string
	err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='gateway'").Scan(&previous)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if previous != "" {
		var old GatewayState
		if err = json.Unmarshal([]byte(previous), &old); err != nil {
			return err
		}
		if old.SessionID == state.SessionID && state.Sequence <= old.Sequence {
			return nil
		}
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES('gateway',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(b)); err != nil {
		return err
	}
	if eventType != "READY" && eventType != "RESUMED" {
		_, err = tx.ExecContext(ctx, "INSERT INTO events(id,session_id,sequence,event_type,data,received_at) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING", EventID(state.SessionID, state.Sequence), state.SessionID, state.Sequence, eventType, []byte(data), protocol.Time(now))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) AddTestEvent(ctx context.Context, eventType string, data json.RawMessage, supplied map[string]any, triggerIDs ...string) (Event, error) {
	id := protocol.NewID("ev")
	b, err := protocol.JSON(supplied)
	if err != nil {
		return Event{}, err
	}
	ev := Event{ID: id, SessionID: "test:" + id, Sequence: 0, EventType: eventType, Data: data, ReceivedAt: protocol.Time(time.Now()), SuppliedContext: b, TriggerIDs: triggerIDs}
	ids, err := json.Marshal(triggerIDs)
	if err != nil {
		return Event{}, err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO events(id,session_id,sequence,event_type,data,received_at,supplied_context,trigger_ids) VALUES(?,?,?,?,?,?,?,?)", ev.ID, ev.SessionID, ev.Sequence, ev.EventType, []byte(data), ev.ReceivedAt, b, ids)
	return ev, err
}
func (s *Store) NextEvent(ctx context.Context, now time.Time) (Event, error) {
	var ev Event
	var ids, supplied []byte
	err := s.db.QueryRowContext(ctx, "SELECT id,session_id,sequence,event_type,data,received_at,supplied_context,last_error,trigger_ids FROM events WHERE next_attempt_at<=? ORDER BY rowid LIMIT 1", now.UnixMilli()).Scan(&ev.ID, &ev.SessionID, &ev.Sequence, &ev.EventType, &ev.Data, &ev.ReceivedAt, &supplied, &ev.LastError, &ids)
	ev.SuppliedContext = supplied
	if err == nil && len(ids) > 0 {
		err = json.Unmarshal(ids, &ev.TriggerIDs)
	}
	return ev, err
}
func (s *Store) RetryEvent(ctx context.Context, id string, cause error) error {
	_, err := s.db.ExecContext(ctx, "UPDATE events SET next_attempt_at=?,last_error=? WHERE id=?", time.Now().Add(10*time.Second).UnixMilli(), cause.Error(), id)
	return err
}
func (s *Store) CommitEvent(ctx context.Context, id string, jobs []NewDelivery) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, job := range jobs {
		e := job.Envelope
		b, err := protocol.JSON(e)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO deliveries(
			id,event_id,trigger_id,trigger_name,condition_snapshot,destination_id,destination_name,
			url_snapshot,result_public_key,result_key_id,event_type,payload,created_at)
			SELECT ?,?,?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM triggers WHERE id=?)
			ON CONFLICT(id) DO NOTHING`,
			e.DeliveryID, e.EventID, e.TriggerID, job.Trigger.Name, job.Trigger.Condition, e.DestinationID, job.Destination.Name,
			job.Destination.URL, job.Destination.ResultPublicKey, job.Destination.KeyID, e.EventType, b, e.Timestamps.EC2ReceivedAt, e.TriggerID)
		if err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM events WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Channel(ctx context.Context, id string) (map[string]any, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT data FROM channels WHERE id=?", id).Scan(&b)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	err = json.Unmarshal(b, &obj)
	return obj, err
}

const deliverySummaryColumns = `id,event_id,trigger_id,trigger_name,condition_snapshot,destination_id,destination_name,
url_snapshot,result_public_key,result_key_id,event_type,state,created_at,first_sent_at,worker_accepted_at,
worker_finished_at,result_received_at,stopped_at,stop_reason,attempts,last_http_status,last_error,
json_extract(CAST(result AS TEXT),'$.status')`

const deliveryColumns = deliverySummaryColumns + `,payload,result`

func scanDelivery(row scanner) (Delivery, error) {
	var d Delivery
	var payload, result []byte
	var status sql.NullString
	err := row.Scan(&d.ID, &d.EventID, &d.TriggerID, &d.TriggerName, &d.Condition, &d.DestinationID, &d.DestinationName, &d.URL, &d.PublicKey, &d.KeyID, &d.EventType, &d.State, &d.CreatedAt, &d.Timestamps.EC2SentAt, &d.Timestamps.WorkerAcceptedAt, &d.Timestamps.WorkerFinishedAt, &d.Timestamps.EC2ResultReceivedAt, &d.StoppedAt, &d.StopReason, &d.Attempts, &d.LastHTTPStatus, &d.LastError, &status, &payload, &result)
	if status.Valid {
		value := protocol.ResultStatus(status.String)
		d.ResultStatus = &value
	}
	d.Payload, d.Result = payload, result
	d.Timestamps.EC2ReceivedAt = d.CreatedAt
	return d, err
}
func (s *Store) Delivery(ctx context.Context, id string) (Delivery, error) {
	return scanDelivery(s.db.QueryRowContext(ctx, "SELECT "+deliveryColumns+" FROM deliveries WHERE id=?", id))
}
func (s *Store) Deliveries(ctx context.Context, destinationID, state, before string, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+deliverySummaryColumns+",NULL,NULL FROM deliveries WHERE (?='' OR destination_id=?) AND (?='' OR state=?) AND (?='' OR rowid<(SELECT rowid FROM deliveries WHERE id=?)) ORDER BY rowid DESC LIMIT ?", destinationID, destinationID, state, state, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) ReadyDestinations(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT destination_id FROM deliveries WHERE state='pending' AND next_attempt_at<=? AND lease_until<=? GROUP BY destination_id ORDER BY MIN(created_at) LIMIT 32", now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (s *Store) PrepareDelivery(ctx context.Context, destinationID string, now time.Time) (Delivery, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, err
	}
	defer tx.Rollback()
	d, err := scanDelivery(tx.QueryRowContext(ctx, "SELECT "+deliveryColumns+" FROM deliveries WHERE destination_id=? AND state='pending' AND next_attempt_at<=? AND lease_until<=? ORDER BY rowid LIMIT 1", destinationID, now.UnixMilli(), now.UnixMilli()))
	if err != nil {
		return d, err
	}
	var targetURL, key, keyID string
	err = tx.QueryRowContext(ctx, "SELECT url,result_public_key,result_key_id FROM destinations WHERE id=? AND EXISTS(SELECT 1 FROM triggers WHERE id=?)", destinationID, d.TriggerID).Scan(&targetURL, &key, &keyID)
	if err != nil {
		return d, err
	}
	if d.Timestamps.EC2SentAt == "" {
		var envelope protocol.Envelope
		if err = json.Unmarshal(d.Payload, &envelope); err != nil {
			return d, err
		}
		d.Timestamps.EC2SentAt = protocol.Time(now)
		envelope.Timestamps.EC2SentAt = d.Timestamps.EC2SentAt
		d.Payload, err = protocol.JSON(envelope)
		if err != nil {
			return d, err
		}
		d.URL = targetURL
		d.PublicKey = key
		d.KeyID = keyID
	}
	d.Attempts++
	_, err = tx.ExecContext(ctx, "UPDATE deliveries SET payload=?,first_sent_at=?,url_snapshot=?,result_public_key=?,result_key_id=?,attempts=?,lease_until=? WHERE id=?", []byte(d.Payload), d.Timestamps.EC2SentAt, d.URL, d.PublicKey, d.KeyID, d.Attempts, now.Add(30*time.Second).UnixMilli(), d.ID)
	if err != nil {
		return d, err
	}
	return d, tx.Commit()
}
func (s *Store) RecordFailure(ctx context.Context, id string, status int, message string, next time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE deliveries SET lease_until=0,next_attempt_at=?,last_http_status=?,last_error=? WHERE id=? AND state='pending' AND result IS NULL", next.UnixMilli(), status, message, id)
	return err
}
func (s *Store) RecordAcceptance(ctx context.Context, id string, a protocol.Acceptance) error {
	if a.DeliveryID != id {
		return errors.New("acceptance delivery_id mismatch")
	}
	t, err := time.Parse(time.RFC3339Nano, a.Timestamps.WorkerAcceptedAt)
	if err != nil {
		return errors.New("acceptance missing valid worker_accepted_at")
	}
	_, err = s.db.ExecContext(ctx, "UPDATE deliveries SET state='accepted',worker_accepted_at=?,lease_until=0,last_http_status=202,last_error='' WHERE id=? AND state IN ('pending','stopped') AND result IS NULL", protocol.Time(t), id)
	return err
}
func (s *Store) ResultKey(ctx context.Context, keyID string) (ed25519.PublicKey, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT result_public_key FROM destinations WHERE result_key_id=?
		UNION ALL SELECT j.result_public_key FROM deliveries j WHERE j.result_key_id=?
		AND j.first_sent_at<>'' LIMIT 1`, keyID, keyID).Scan(&encoded)
	if err != nil {
		return nil, err
	}
	return protocol.DecodePublicKey(encoded)
}
func (s *Store) RecordResult(ctx context.Context, r protocol.Result, signingKeyID string, now time.Time) (bool, error) {
	canonical, err := protocol.CanonicalResult(r)
	if err != nil {
		return false, err
	}
	hash := protocol.Digest(canonical)
	a, _ := time.Parse(time.RFC3339Nano, r.Timestamps.WorkerAcceptedAt)
	f, _ := time.Parse(time.RFC3339Nano, r.Timestamps.WorkerFinishedAt)
	result, err := s.db.ExecContext(ctx, `UPDATE deliveries SET state='finished',worker_accepted_at=?,worker_finished_at=?,
		result_received_at=?,result=?,result_hash=?,lease_until=0,last_error=''
		WHERE id=? AND destination_id=? AND result_key_id=? AND first_sent_at<>'' AND result IS NULL`,
		protocol.Time(a), protocol.Time(f), protocol.Time(now), canonical, hash, r.DeliveryID, r.DestinationID, signingKeyID)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	if n == 1 {
		return true, nil
	}
	var existing, firstSent, dest, keyID string
	err = s.db.QueryRowContext(ctx, "SELECT result_hash,first_sent_at,destination_id,result_key_id FROM deliveries WHERE id=?", r.DeliveryID).Scan(&existing, &firstSent, &dest, &keyID)
	if err != nil {
		return false, err
	}
	if dest != r.DestinationID {
		return false, ErrResultOwner
	}
	if firstSent == "" {
		return false, ErrNotSent
	}
	if keyID != signingKeyID {
		return false, ErrResultOwner
	}
	if existing == hash {
		return false, nil
	}
	return false, ErrConflict
}
func (s *Store) StopDelivery(ctx context.Context, id string) error {
	r, err := s.db.ExecContext(ctx, "UPDATE deliveries SET state='stopped',stopped_at=?,stop_reason='operator_cancelled',lease_until=0 WHERE id=? AND state='pending'", protocol.Time(time.Now()), id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		if _, err = s.Delivery(ctx, id); err != nil {
			return err
		}
		return errors.New("only unaccepted deliveries can be cancelled")
	}
	return nil
}
func (s *Store) Stats(ctx context.Context, now time.Time) (Stats, error) {
	var out Stats
	rows, err := s.db.QueryContext(ctx, "SELECT state,COUNT(*) FROM deliveries GROUP BY state")
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var state string
		var count int
		if err = rows.Scan(&state, &count); err != nil {
			rows.Close()
			return out, err
		}
		switch state {
		case "pending":
			out.Pending = count
		case "accepted":
			out.AwaitingResult = count
		case "finished":
			out.Finished = count
		case "stopped":
			out.Stopped = count
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&out.QueuedEvents); err != nil {
		return out, err
	}
	var oldest sql.NullString
	if err = s.db.QueryRowContext(ctx, "SELECT MIN(created_at) FROM deliveries WHERE state='pending'").Scan(&oldest); err != nil {
		return out, err
	}
	if oldest.Valid {
		t, err := time.Parse(time.RFC3339Nano, oldest.String)
		if err != nil {
			return out, err
		}
		out.OldestPendingSeconds = max(0, now.Sub(t).Seconds())
	}
	return out, nil
}
func (s *Store) Cleanup(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM deliveries WHERE (state='finished' AND result_received_at<?) OR (state='stopped' AND stopped_at<?)", protocol.Time(before), protocol.Time(before))
	return err
}
func (s *Store) Backup(ctx context.Context, path string) error {
	return s.BackupLimited(ctx, path, math.MaxInt64)
}

var ErrSnapshotTooLarge = errors.New("snapshot exceeds available backup capacity")

func (s *Store) BackupLimited(ctx context.Context, path string, maxBytes int64) error {
	size, err := s.SnapshotSize(ctx)
	if err != nil {
		return err
	}
	if maxBytes <= 0 || size > maxBytes {
		return ErrSnapshotTooLarge
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	file.Close()
	complete := false
	defer func() {
		if !complete {
			os.Remove(path)
		}
	}()
	dest, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer dest.Close()
	source, err := sql.Open("sqlite3", "file:"+s.path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer source.Close()
	dst, err := dest.Conn(ctx)
	if err != nil {
		return err
	}
	defer dst.Close()
	src, err := source.Conn(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	var pageSize int64
	if err = src.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return err
	}
	err = dst.Raw(func(d any) error {
		return src.Raw(func(s any) error {
			backup, err := d.(*sqlite3.SQLiteConn).Backup("main", s.(*sqlite3.SQLiteConn), "main")
			if err != nil {
				return err
			}
			defer backup.Close()
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				done, err := backup.Step(128)
				if err != nil {
					return err
				}
				if int64(backup.PageCount())*pageSize > maxBytes {
					return ErrSnapshotTooLarge
				}
				if done {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
		})
	})
	if err != nil {
		return fmt.Errorf("SQLite backup: %w", err)
	}
	complete = true
	return nil
}
