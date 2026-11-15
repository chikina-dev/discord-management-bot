package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/store"
)

func TestBodylessDeleteRequiresContentType(t *testing.T) {
	_, admin, _, _ := newService(t)
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	var destination store.Destination
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/destinations", destinationRequest{Name: "bodyless-delete", URL: "https://example.invalid/events", ResultPublicKey: protocol.EncodePublicKey(public)}, 201), &destination)
	req, err := http.NewRequest(http.MethodDelete, admin.URL+"/destinations/"+destination.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := admin.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 415 {
		t.Fatalf("bodyless DELETE without Content-Type: %d", response.StatusCode)
	}
	request(t, admin.Client(), "GET", admin.URL+"/destinations/"+destination.ID, nil, 200)
	request(t, admin.Client(), "DELETE", admin.URL+"/destinations/"+destination.ID, nil, 204)
	request(t, admin.Client(), "GET", admin.URL+"/destinations/"+destination.ID, nil, 404)
}

func TestUnmatchedTestEventReturnsCandidateID(t *testing.T) {
	a, admin, _, ctx := newService(t)
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	var destination store.Destination
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/destinations", destinationRequest{Name: "candidate-only", URL: "https://example.invalid/events", ResultPublicKey: protocol.EncodePublicKey(public)}, 201), &destination)
	var trigger store.Trigger
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/triggers", triggerRequest{Name: "never", DestinationID: destination.ID, Condition: "false"}, 201), &trigger)
	var queued struct {
		EventID     string   `json:"event_id"`
		DeliveryIDs []string `json:"delivery_ids"`
	}
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/tests/events", testEventRequest{EventType: "MESSAGE_CREATE", Data: json.RawMessage(`{"id":"candidate-only"}`), TriggerIDs: []string{trigger.ID}}, 202), &queued)
	if len(queued.DeliveryIDs) != 1 || queued.DeliveryIDs[0] != store.DeliveryID(queued.EventID, trigger.ID) {
		t.Fatalf("unexpected candidate response: %#v", queued)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		stats, err := a.Store.Stats(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if stats.QueuedEvents == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test event was not evaluated")
		}
		time.Sleep(10 * time.Millisecond)
	}
	request(t, admin.Client(), "GET", admin.URL+"/deliveries/"+queued.DeliveryIDs[0], nil, 404)
	items, err := a.Store.Deliveries(ctx, destination.ID, "", "", 100)
	if err != nil || len(items) != 0 {
		t.Fatalf("unmatched event created a delivery: %v %v", items, err)
	}
}
