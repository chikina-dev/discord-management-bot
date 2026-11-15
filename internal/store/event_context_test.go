package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func contextFor(name string) EventContextSnapshot {
	return EventContextSnapshot{Data: map[string]any{"channel": map[string]any{"id": "channel", "name": name}}, Complete: true}
}
func channelFor(name string) []map[string]any {
	return []map[string]any{{"id": "channel", "name": name}}
}

func TestFailedFirstContextCaptureCannotRewindNewerProjection(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	for _, sequence := range []int64{1, 2} {
		if err := s.SaveDispatch(ctx, GatewayState{SessionID: "session", Sequence: sequence}, "CHANNEL_UPDATE", json.RawMessage(`{"id":"channel","guild_id":"guild"}`), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	oldID, newID := EventID("session", 1), EventID("session", 2)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_context BEFORE INSERT ON event_contexts BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CaptureEventContext(ctx, oldID, "guild", channelFor("old"), contextFor("old")); err == nil {
		t.Fatal("capture failure was not injected")
	}
	if _, err := s.Channel(ctx, "channel"); err != ErrNotFound {
		t.Fatalf("failed capture leaked its channel write: %v", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_context"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CaptureEventContext(ctx, newID, "guild", channelFor("new"), contextFor("new")); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitEvent(ctx, newID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CaptureEventContext(ctx, oldID, "guild", channelFor("old"), contextFor("old")); err != nil {
		t.Fatal(err)
	}
	channel, err := s.Channel(ctx, "channel")
	if err != nil || channel["name"] != "new" {
		t.Fatalf("first capture retry rewound newer state: %+v %v", channel, err)
	}
	snapshot, err := s.EventContext(ctx, oldID)
	if err != nil || snapshot.Data["channel"].(map[string]any)["name"] != "old" {
		t.Fatalf("old decision snapshot was not preserved: %+v %v", snapshot, err)
	}

	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if snapshot, err := restored.EventContext(ctx, oldID); err != nil || !snapshot.Complete {
		t.Fatalf("backup lost pending context: %+v %v", snapshot, err)
	}
	if err := restored.CommitEvent(ctx, oldID, nil); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"events", "event_positions", "event_contexts"} {
		var count int
		if err := restored.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("completed event leaked %s: count=%d %v", table, count, err)
		}
	}
	if err := restored.SaveDispatch(ctx, GatewayState{SessionID: "new-session", Sequence: 1}, "CHANNEL_UPDATE", json.RawMessage(`{"id":"channel","guild_id":"guild"}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.CaptureEventContext(ctx, EventID("new-session", 1), "guild", channelFor("future"), contextFor("future")); err != nil {
		t.Fatal(err)
	}
	channel, err = restored.Channel(ctx, "channel")
	if err != nil || channel["name"] != "future" {
		t.Fatalf("empty queue or new session reset durable order: %+v %v", channel, err)
	}
}

func TestContextCompletionDoesNotReplayChannelProjection(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	for _, sequence := range []int64{1, 2} {
		if err := s.SaveDispatch(ctx, GatewayState{SessionID: "session", Sequence: sequence}, "CHANNEL_UPDATE", json.RawMessage(`{"id":"channel","guild_id":"guild"}`), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	partial := contextFor("old")
	partial.Complete = false
	if _, err := s.CaptureEventContext(ctx, EventID("session", 1), "guild", channelFor("old"), partial); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CaptureEventContext(ctx, EventID("session", 2), "guild", channelFor("new"), contextFor("new")); err != nil {
		t.Fatal(err)
	}
	completed, err := s.CaptureEventContext(ctx, EventID("session", 1), "guild", channelFor("old"), contextFor("enriched"))
	if err != nil || !completed.Complete {
		t.Fatalf("snapshot was not completed: %+v %v", completed, err)
	}
	channel, err := s.Channel(ctx, "channel")
	if err != nil || channel["name"] != "new" {
		t.Fatalf("completion replayed old projection: %+v %v", channel, err)
	}
}

func TestExistingQueuedEventsReceiveDurableOrderOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE events (
id TEXT PRIMARY KEY, session_id TEXT NOT NULL, sequence INTEGER NOT NULL,
event_type TEXT NOT NULL, data BLOB NOT NULL, received_at TEXT NOT NULL,
supplied_context BLOB, trigger_ids BLOB, next_attempt_at INTEGER NOT NULL DEFAULT 0,
last_error TEXT NOT NULL DEFAULT '', UNIQUE(session_id,sequence));
INSERT INTO events(id,session_id,sequence,event_type,data,received_at) VALUES
('old','legacy',10,'CHANNEL_UPDATE','{}','2026-01-01T00:00:00Z'),
('new','legacy',11,'CHANNEL_UPDATE','{}','2026-01-01T00:00:01Z');`)
	legacy.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.CaptureEventContext(ctx, "new", "guild", channelFor("new"), contextFor("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CaptureEventContext(ctx, "old", "guild", channelFor("old"), contextFor("old")); err != nil {
		t.Fatal(err)
	}
	channel, err := s.Channel(ctx, "channel")
	if err != nil || channel["name"] != "new" {
		t.Fatalf("legacy event order was not backfilled: %+v %v", channel, err)
	}
}
