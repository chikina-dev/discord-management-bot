package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/store"
)

type committedChangeContext struct {
	context.Context
	cancel context.CancelFunc
	probe  *sql.DB
	id     string
}

func (c *committedChangeContext) observeCommit() {
	var condition string
	if err := c.probe.QueryRowContext(context.Background(), "SELECT condition FROM triggers WHERE id=?", c.id).Scan(&condition); err == nil && condition == "false" {
		c.cancel()
	}
}
func (c *committedChangeContext) Done() <-chan struct{} {
	c.observeCommit()
	return c.Context.Done()
}
func (c *committedChangeContext) Err() error {
	c.observeCommit()
	return c.Context.Err()
}

func consistencyApp(t *testing.T, client *http.Client) (*App, *store.Store, store.Destination, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(db, Config{Key: key, GuildID: "guild", BotToken: "fixture", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	destination, err := a.putDestination(context.Background(), store.Destination{Name: "worker", URL: "https://worker.example/events", ResultPublicKey: protocol.EncodePublicKey(pub)}, true)
	if err != nil {
		t.Fatal(err)
	}
	return a, db, destination, path
}

func pendingEvent(t *testing.T, db *store.Store, sequence int64, eventType string, data json.RawMessage) store.Event {
	t.Helper()
	if err := db.SaveDispatch(context.Background(), store.GatewayState{SessionID: "session", Sequence: sequence}, eventType, data, time.Now()); err != nil {
		t.Fatal(err)
	}
	event, err := db.NextEvent(context.Background(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestCommittedCatalogChangePublishesAfterRequestCancellation(t *testing.T) {
	a, db, destination, path := consistencyApp(t, nil)
	trigger, err := a.putTrigger(context.Background(), store.Trigger{Name: "rule", DestinationID: destination.ID, Condition: "true"}, true)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &committedChangeContext{Context: base, cancel: cancel, probe: probe, id: trigger.ID}
	body, _ := protocol.JSON(triggerRequest{Name: trigger.Name, DestinationID: destination.ID, Condition: "false"})
	request := httptest.NewRequest("PUT", "http://127.0.0.1/triggers/"+trigger.ID, bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.ManagementHandler().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("change was not completed: %d %s", response.Code, response.Body.String())
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("request did not become canceled when its committed change was visible")
	}
	saved, err := db.Trigger(context.Background(), trigger.ID)
	if err != nil || saved.Condition != "false" {
		t.Fatalf("durable condition: %+v %v", saved, err)
	}
	event := pendingEvent(t, db, 1, "MESSAGE_CREATE", json.RawMessage(`{"guild_id":"guild"}`))
	if err := a.processEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.Deliveries(context.Background(), destination.ID, "", "", 100)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("stale true condition created a delivery: %+v %v", jobs, err)
	}
}

func TestRetriedChannelEventUsesFrozenContextWithoutRewindingProjection(t *testing.T) {
	a, db, destination, _ := consistencyApp(t, nil)
	_, err := a.putTrigger(context.Background(), store.Trigger{Name: "expensive", DestinationID: destination.ID, Condition: `data.permission_overwrites.every(a => data.permission_overwrites.every(b => data.permission_overwrites.every(c => true)))`}, true)
	if err != nil {
		t.Fatal(err)
	}
	permissions := make([]any, 60)
	for i := range permissions {
		permissions[i] = map[string]any{"id": "role"}
	}
	raw, _ := protocol.JSON(map[string]any{"id": "channel", "guild_id": "guild", "name": "old", "permission_overwrites": permissions})
	old := pendingEvent(t, db, 1, "CHANNEL_UPDATE", raw)
	if err := a.processEvent(context.Background(), old); err == nil {
		t.Fatal("expected evaluation budget error")
	}
	if err := db.RetryEvent(context.Background(), old.ID, errors.New("budget")); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDispatch(context.Background(), store.GatewayState{SessionID: "session", Sequence: 2}, "CHANNEL_UPDATE", json.RawMessage(`{"id":"channel","guild_id":"guild","name":"new","permission_overwrites":[]}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	newer, err := db.NextEvent(context.Background(), time.Now())
	if err != nil || newer.ID == old.ID {
		t.Fatalf("new event not ready: %+v %v", newer, err)
	}
	if err := a.processEvent(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	if err := a.processEvent(context.Background(), old); err == nil {
		t.Fatal("expected retried evaluation budget error")
	}
	channel, err := db.Channel(context.Background(), "channel")
	if err != nil || channel["name"] != "new" {
		t.Fatalf("old retry rewound the projection: %+v %v", channel, err)
	}
	snapshot, err := db.EventContext(context.Background(), old.ID)
	if err != nil || snapshot.Data["channel"].(map[string]any)["name"] != "old" {
		t.Fatalf("retry lost its original context: %+v %v", snapshot, err)
	}
}

func TestRetryEnrichesIncompleteContextAfterCatalogStartsUsingIt(t *testing.T) {
	var lookups atomic.Int64
	client := &http.Client{Transport: metadataTransport(func(r *http.Request) (*http.Response, error) {
		lookups.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"channel","guild_id":"guild","parent_id":"forum"}`))}, nil
	})}
	a, db, destination, _ := consistencyApp(t, client)
	trigger, err := a.putTrigger(context.Background(), store.Trigger{Name: "rule", DestinationID: destination.ID, Condition: `data.items.every(a => data.items.every(b => data.items.every(c => true)))`}, true)
	if err != nil {
		t.Fatal(err)
	}
	items := make([]int, 60)
	raw, _ := protocol.JSON(map[string]any{"guild_id": "guild", "channel_id": "channel", "items": items})
	event := pendingEvent(t, db, 1, "MESSAGE_CREATE", raw)
	if err := a.processEvent(context.Background(), event); err == nil {
		t.Fatal("expected evaluation budget failure")
	}
	snapshot, err := db.EventContext(context.Background(), event.ID)
	if err != nil || snapshot.Complete || lookups.Load() != 0 {
		t.Fatalf("expected an incomplete fallback without network: %+v %v", snapshot, err)
	}
	trigger.Condition = `context.channel.parent_id == "forum"`
	if _, err := a.putTrigger(context.Background(), trigger, false); err != nil {
		t.Fatal(err)
	}
	if err := a.processEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.Deliveries(context.Background(), destination.ID, "", "", 100)
	if err != nil || len(jobs) != 1 || lookups.Load() != 1 {
		t.Fatalf("new context requirement was ignored: %+v %v, lookups=%d", jobs, err, lookups.Load())
	}
	delivery, err := db.Delivery(context.Background(), jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var payload protocol.Envelope
	if err := json.Unmarshal(delivery.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Context["channel"].(map[string]any)["parent_id"] != "forum" {
		t.Fatal("delivery used the incomplete fallback")
	}
}
