package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
)

func setup(t *testing.T) (*Store, Destination, Trigger) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	d, err := s.PutDestination(context.Background(), Destination{Name: "test", URL: "https://worker.example/events", ResultPublicKey: protocol.EncodePublicKey(pub)}, true)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.PutTrigger(context.Background(), Trigger{DestinationID: d.ID, Name: "messages", Condition: `event_type == "MESSAGE_CREATE"`}, true)
	if err != nil {
		t.Fatal(err)
	}
	return s, d, r
}
func enqueue(t *testing.T, s *Store, d Destination, r Trigger) string {
	t.Helper()
	ctx := context.Background()
	ev, err := s.AddTestEvent(ctx, "MESSAGE_CREATE", json.RawMessage(`{"id":"message","attachments":[]}`), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	id := DeliveryID(ev.ID, r.ID)
	env := protocol.Envelope{DeliveryID: id, EventID: ev.ID, TriggerID: r.ID, DestinationID: d.ID, EventType: ev.EventType, Timestamps: protocol.Timestamps{EC2ReceivedAt: ev.ReceivedAt}, Data: ev.Data, Context: map[string]any{}}
	if err := s.CommitEvent(ctx, ev.ID, []NewDelivery{{Envelope: env, Trigger: r, Destination: d}}); err != nil {
		t.Fatal(err)
	}
	return id
}
func final(d Destination, id string, now time.Time) protocol.Result {
	return protocol.Result{DeliveryID: id, DestinationID: d.ID, Status: protocol.Success, Timestamps: protocol.Timestamps{WorkerAcceptedAt: protocol.Time(now), WorkerFinishedAt: protocol.Time(now.Add(time.Second))}, Details: json.RawMessage(`{"count":1}`)}
}
func TestCascadeRetainsHistoryAndLateResults(t *testing.T) {
	s, d, r := setup(t)
	ctx := context.Background()
	waiting := enqueue(t, s, d, r)
	delivery, err := s.PrepareDelivery(ctx, d.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if delivery.ID != waiting {
		t.Fatal("wrong first delivery")
	}
	unsent := enqueue(t, s, d, r)
	if err = s.DeleteDestination(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Trigger(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trigger did not cascade: %v", err)
	}
	for _, id := range []string{waiting, unsent} {
		j, err := s.Delivery(ctx, id)
		if err != nil || j.State != "stopped" {
			t.Fatalf("lost history or continued send: %#v %v", j, err)
		}
	}
	if _, err = s.PrepareDelivery(ctx, d.ID, time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted destination can still receive")
	}
	if _, err = s.ResultKey(ctx, d.KeyID); err != nil {
		t.Fatalf("lost key for in-flight result: %v", err)
	}
	received, err := s.RecordResult(ctx, final(d, waiting, time.Now()), d.KeyID, time.Now().Add(time.Second))
	if err != nil || !received {
		t.Fatalf("late result failed: %v", err)
	}
	if _, err = s.RecordResult(ctx, final(d, unsent, time.Now()), d.KeyID, time.Now()); !errors.Is(err, ErrNotSent) {
		t.Fatalf("accepted result for unsent delivery: %v", err)
	}
}
func TestStableRetryAndFinalResultRace(t *testing.T) {
	s, d, r := setup(t)
	ctx := context.Background()
	id := enqueue(t, s, d, r)
	now := time.Now()
	a, err := s.PrepareDelivery(ctx, d.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordFailure(ctx, id, 503, "unavailable", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	b, err := s.PrepareDelivery(ctx, d.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || string(a.Payload) != string(b.Payload) || b.Attempts != 2 {
		t.Fatal("retry changed identity or payload")
	}
	result := final(d, id, now)
	first, err := s.RecordResult(ctx, result, d.KeyID, now.Add(time.Second))
	if err != nil || !first {
		t.Fatal(err)
	}
	duplicate, err := s.RecordResult(ctx, result, d.KeyID, now.Add(time.Minute))
	if err != nil || duplicate {
		t.Fatal("duplicate result counted again")
	}
	if err = s.RecordFailure(ctx, id, 0, "late timeout", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordAcceptance(ctx, id, protocol.Acceptance{DeliveryID: id, Timestamps: protocol.Timestamps{WorkerAcceptedAt: protocol.Time(now)}}); err != nil {
		t.Fatal(err)
	}
	j, err := s.Delivery(ctx, id)
	if err != nil || j.State != "finished" || j.LastError != "" {
		t.Fatalf("late response regressed state: %#v %v", j, err)
	}
	result.Status = protocol.Failure
	if _, err = s.RecordResult(ctx, result, d.KeyID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrote final result: %v", err)
	}
}
func TestDurableCursorAndBackup(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	state := GatewayState{SessionID: "session", Sequence: 10, Fingerprint: "f"}
	data := json.RawMessage(`{"id":"123","guild_id":"g"}`)
	if err := s.SaveDispatch(ctx, state, "MESSAGE_CREATE", data, time.Now()); err != nil {
		t.Fatal(err)
	}
	event, err := s.NextEvent(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if event.ID != EventID("session", 10) {
		t.Fatal("unstable event identity")
	}
	if err = s.CommitEvent(ctx, event.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveDispatch(ctx, state, "MESSAGE_CREATE", data, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextEvent(ctx, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatal("replayed processed event was duplicated")
	}
	state.Sequence = 11
	if err = s.SaveDispatch(ctx, state, "MESSAGE_CREATE", data, time.Now()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err = s.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.GatewayState(ctx)
	if err != nil || got.Sequence != 11 {
		t.Fatal("backup lost cursor")
	}
	pending, err := restored.NextEvent(ctx, time.Now())
	if err != nil || pending.Sequence != 11 {
		t.Fatal("backup lost pending event")
	}
}
func TestDeletionDoesNotAffectOtherDestination(t *testing.T) {
	s, d, r := setup(t)
	ctx := context.Background()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	other, err := s.PutDestination(ctx, Destination{Name: "other", URL: "https://other.example/", ResultPublicKey: protocol.EncodePublicKey(pub)}, true)
	if err != nil {
		t.Fatal(err)
	}
	otherTrigger, err := s.PutTrigger(ctx, Trigger{DestinationID: other.ID, Name: "messages", Condition: "true"}, true)
	if err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, d, r)
	otherID := enqueue(t, s, other, otherTrigger)
	if err = s.DeleteDestination(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Trigger(ctx, otherTrigger.ID); err != nil {
		t.Fatal(err)
	}
	j, err := s.Delivery(ctx, otherID)
	if err != nil || j.State != "pending" {
		t.Fatal("affected unrelated destination")
	}
	if _, err = s.PutTrigger(ctx, Trigger{DestinationID: "missing", Name: "bad", Condition: "true"}, true); err == nil {
		t.Fatal("foreign keys are disabled")
	}
}
func TestPaginationAndRetention(t *testing.T) {
	s, d, r := setup(t)
	ctx := context.Background()
	for range 23 {
		enqueue(t, s, d, r)
	}
	first, err := s.Deliveries(ctx, "", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Deliveries(ctx, "", "", first[len(first)-1].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(first)+len(second) != 23 {
		t.Fatalf("lost tied-timestamp records: %d + %d", len(first), len(second))
	}
	if err = s.Cleanup(ctx, time.Now().AddDate(2, 0, 0)); err != nil {
		t.Fatal(err)
	}
	stat, err := s.Stats(ctx, time.Now())
	if err != nil || stat.Pending != 23 {
		t.Fatal("pending deliveries were deleted")
	}
}
