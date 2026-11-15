package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/store"
)

func request(t *testing.T, client *http.Client, method, target string, body any, status int) []byte {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := protocol.JSON(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	b, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s: got %d: %s", method, target, response.StatusCode, b)
	}
	return b
}
func newService(t *testing.T) (*App, *httptest.Server, *httptest.Server, context.Context) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	a, err := New(s, Config{Key: key, GuildID: "guild", GuildName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	pub := httptest.NewServer(a.PublicHandler())
	a.cfg.ResultURL = pub.URL + "/results"
	admin := httptest.NewServer(a.ManagementHandler())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; admin.Close(); pub.Close(); s.Close() })
	return a, admin, pub, ctx
}

func TestManagementAndEndToEndFourResults(t *testing.T) {
	a, admin, public, ctx := newService(t)
	workerPublic, workerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	servicePublic := a.cfg.Key.Public().(ed25519.PublicKey)
	var requests atomic.Int64
	var workerErrors atomic.Int64
	var processing sync.Mutex
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if protocol.Verify(servicePublic, r.Method, protocol.RequestURL(r), body, r.Header, time.Now()) != nil {
			workerErrors.Add(1)
			w.WriteHeader(401)
			return
		}
		var envelope protocol.Envelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			workerErrors.Add(1)
			w.WriteHeader(400)
			return
		}
		requests.Add(1)
		var original map[string]any
		json.Unmarshal(envelope.Data, &original)
		if original["timestamp"] != "2020-01-01T00:00:00Z" {
			workerErrors.Add(1)
		}
		if files, ok := original["attachments"].([]any); !ok || len(files) != 3 {
			workerErrors.Add(1)
		}
		if envelope.Attachments == nil || len(*envelope.Attachments) != 2 {
			workerErrors.Add(1)
		}
		accepted := protocol.Time(time.Now())
		respond(w, 202, protocol.Acceptance{DeliveryID: envelope.DeliveryID, Timestamps: protocol.Timestamps{WorkerAcceptedAt: accepted}})
		go func() {
			processing.Lock()
			defer processing.Unlock()
			result := protocol.Result{DeliveryID: envelope.DeliveryID, DestinationID: envelope.DestinationID, Status: protocol.ResultStatus(original["test_status"].(string)), Timestamps: protocol.Timestamps{WorkerAcceptedAt: accepted, WorkerFinishedAt: protocol.Time(time.Now())}, Details: json.RawMessage(`{"selected":2}`)}
			b, _ := protocol.JSON(result)
			u, _ := url.Parse(public.URL + "/results")
			req, _ := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(b))
			req.Header = protocol.Sign(workerPrivate, "POST", u, b, time.Now())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				if ctx.Err() == nil {
					workerErrors.Add(1)
				}
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 204 {
				workerErrors.Add(1)
			}
		}()
	}))
	defer receiver.Close()
	var destination store.Destination
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/destinations", destinationRequest{Name: "fixture", URL: receiver.URL + "/events", ResultPublicKey: protocol.EncodePublicKey(workerPublic)}, 201), &destination)
	expression := `event_type == "MESSAGE_CREATE" && data.author.bot != true && data.attachments.some(a => a.content_type == "application/pdf")`
	var trigger store.Trigger
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/triggers", triggerRequest{DestinationID: destination.ID, Name: "pdf-files", Condition: expression}, 201), &trigger)
	var unrelated store.Trigger
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/triggers", triggerRequest{DestinationID: destination.ID, Name: "must-not-run", Condition: "true"}, 201), &unrelated)
	for _, status := range []protocol.ResultStatus{protocol.Success, protocol.PartialSuccess, protocol.Failure, protocol.Skipped} {
		body := testEventRequest{EventType: "MESSAGE_CREATE", Data: json.RawMessage(`{"timestamp":"2020-01-01T00:00:00Z","author":{"id":"human"},"attachments":[{"id":"1","content_type":"application/pdf"},{"id":"2","content_type":"image/png"},{"id":"3","content_type":"application/pdf"}],"test_status":"` + string(status) + `"}`), Context: map[string]any{"channel": map[string]any{"parent_id": "forum"}}, TriggerIDs: []string{trigger.ID}}
		var queued struct {
			DeliveryIDs []string `json:"delivery_ids"`
		}
		json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/tests/events", body, 202), &queued)
		if len(queued.DeliveryIDs) != 1 {
			t.Fatal("missing test delivery")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			job, err := a.Store.Delivery(ctx, queued.DeliveryIDs[0])
			if err == nil && job.State == "finished" {
				var result protocol.Result
				json.Unmarshal(job.Result, &result)
				if result.Status != status {
					t.Fatalf("wrong outcome: %s", job.Result)
				}
				ts := job.Timestamps
				if ts.EC2ReceivedAt == "" || ts.EC2SentAt == "" || ts.WorkerAcceptedAt == "" || ts.WorkerFinishedAt == "" || ts.EC2ResultReceivedAt == "" {
					t.Fatalf("missing timeline %#v", ts)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("result did not arrive: %#v %v", job, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if requests.Load() != 4 || workerErrors.Load() != 0 {
		t.Fatalf("requests=%d errors=%d", requests.Load(), workerErrors.Load())
	}
	request(t, admin.Client(), "PUT", admin.URL+"/triggers/"+trigger.ID, triggerRequest{DestinationID: destination.ID, Name: trigger.Name, Condition: "broken("}, 400)
	retained, err := a.Store.Trigger(ctx, trigger.ID)
	if err != nil || retained.Condition != expression {
		t.Fatal("invalid update modified active trigger")
	}
	request(t, admin.Client(), "DELETE", admin.URL+"/destinations/"+destination.ID, nil, 204)
	request(t, admin.Client(), "GET", admin.URL+"/triggers/"+trigger.ID, nil, 404)
	request(t, admin.Client(), "GET", admin.URL+"/triggers/"+unrelated.ID, nil, 404)
	history, err := a.Store.Deliveries(ctx, destination.ID, "", "", 100)
	if err != nil || len(history) != 4 {
		t.Fatal("cascade lost history")
	}
	request(t, public.Client(), "GET", public.URL+"/destinations", nil, 404)
	req, _ := http.NewRequest("POST", admin.URL+"/tests/events", bytes.NewReader([]byte("{}")))
	req.Header.Set("Origin", "https://untrusted.example")
	req.Header.Set("Content-Type", "application/json")
	resp, err := admin.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("browser-origin mutation accepted")
	}
}

func TestLostAcceptanceRetriesSameDeliveryWithoutRestartingWork(t *testing.T) {
	a, admin, public, ctx := newService(t)
	pub, workerKey, _ := ed25519.GenerateKey(rand.Reader)
	var attempts, started atomic.Int64
	var mu sync.Mutex
	var firstBody []byte
	accepted := ""
	var envelope protocol.Envelope
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if protocol.Verify(a.cfg.Key.Public().(ed25519.PublicKey), r.Method, protocol.RequestURL(r), b, r.Header, time.Now()) != nil {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		n := attempts.Add(1)
		if n == 1 {
			started.Add(1)
			firstBody = append([]byte{}, b...)
			json.Unmarshal(b, &envelope)
			accepted = protocol.Time(time.Now())
			w.WriteHeader(503)
			return
		}
		if string(firstBody) != string(b) {
			t.Error("body changed during retry")
			w.WriteHeader(409)
			return
		}
		respond(w, 202, protocol.Acceptance{DeliveryID: envelope.DeliveryID, Timestamps: protocol.Timestamps{WorkerAcceptedAt: accepted}})
	}))
	defer receiver.Close()
	var dest store.Destination
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/destinations", destinationRequest{Name: "retry-fixture", URL: receiver.URL + "/events", ResultPublicKey: protocol.EncodePublicKey(pub)}, 201), &dest)
	var trigger store.Trigger
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/triggers", triggerRequest{DestinationID: dest.ID, Name: "all", Condition: "true"}, 201), &trigger)
	var queued struct {
		DeliveryIDs []string `json:"delivery_ids"`
	}
	json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/tests/events", testEventRequest{EventType: "MESSAGE_CREATE", Data: json.RawMessage(`{"id":"1"}`), TriggerIDs: []string{trigger.ID}}, 202), &queued)
	id := queued.DeliveryIDs[0]
	deadline := time.Now().Add(6 * time.Second)
	for {
		job, err := a.Store.Delivery(ctx, id)
		if err == nil && job.State == "accepted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retry not accepted: %#v %v", job, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if attempts.Load() != 2 || started.Load() != 1 {
		t.Fatal("duplicate business processing")
	}
	result := protocol.Result{DeliveryID: id, DestinationID: dest.ID, Status: protocol.Success, Timestamps: protocol.Timestamps{WorkerAcceptedAt: accepted, WorkerFinishedAt: protocol.Time(time.Now())}}
	body, _ := protocol.JSON(result)
	u, _ := url.Parse(public.URL + "/results")
	for range 2 {
		req, _ := http.NewRequest("POST", u.String(), bytes.NewReader(body))
		req.Header = protocol.Sign(workerKey, "POST", u, body, time.Now())
		resp, err := public.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatal("result notification not idempotent")
		}
	}
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	req, _ := http.NewRequest("POST", u.String(), bytes.NewReader(body))
	req.Header = protocol.Sign(otherKey, "POST", u, body, time.Now())
	resp, err := public.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("unregistered result signer accepted")
	}
}
