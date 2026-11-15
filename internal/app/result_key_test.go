package app

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/store"
)

func TestInFlightResultKeepsOriginalKeyAfterRegistrationUpdate(t *testing.T) {
	service, admin, public, _ := newService(t)
	oldPublic, oldPrivate, _ := ed25519.GenerateKey(rand.Reader)
	newPublic, newPrivate, _ := ed25519.GenerateKey(rand.Reader)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope protocol.Envelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(protocol.Acceptance{DeliveryID: envelope.DeliveryID, Timestamps: protocol.Timestamps{WorkerAcceptedAt: protocol.Time(time.Now())}})
	}))
	defer receiver.Close()
	var destination store.Destination
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/destinations", destinationRequest{Name: "rotate", URL: receiver.URL, ResultPublicKey: protocol.EncodePublicKey(oldPublic)}, 201), &destination)
	var trigger store.Trigger
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/triggers", triggerRequest{Name: "all", DestinationID: destination.ID, Condition: "true"}, 201), &trigger)
	var queued struct {
		DeliveryIDs []string `json:"delivery_ids"`
	}
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/tests/events", testEventRequest{EventType: "MESSAGE_CREATE", Data: json.RawMessage(`{"id":"rotation"}`), TriggerIDs: []string{trigger.ID}}, 202), &queued)
	id := queued.DeliveryIDs[0]
	deadline := time.Now().Add(3 * time.Second)
	for {
		job, err := service.Store.Delivery(t.Context(), id)
		if err == nil && job.State == "accepted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not accepted: %+v %v", job, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	request(t, admin.Client(), "PUT", admin.URL+"/destinations/"+destination.ID, destinationRequest{Name: "rotate", URL: receiver.URL + "/new", ResultPublicKey: protocol.EncodePublicKey(newPublic)}, 200)
	now := protocol.Time(time.Now())
	result := protocol.Result{DeliveryID: id, DestinationID: destination.ID, Status: protocol.Success, Timestamps: protocol.Timestamps{WorkerAcceptedAt: now, WorkerFinishedAt: now}}
	body, _ := protocol.JSON(result)
	target, _ := url.Parse(public.URL + "/results")
	send := func(key ed25519.PrivateKey, expected int) {
		t.Helper()
		req, _ := http.NewRequest("POST", target.String(), bytes.NewReader(body))
		req.Header = protocol.Sign(key, "POST", target, body, time.Now())
		response, err := public.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != expected {
			t.Fatalf("callback status %d want %d: %s", response.StatusCode, expected, b)
		}
	}
	send(newPrivate, 403)
	send(oldPrivate, 204)
	request(t, admin.Client(), "DELETE", admin.URL+"/destinations/"+destination.ID, nil, 204)
	send(oldPrivate, 204)
}
