package test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/app"
	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/store"
	"github.com/chikina-dev/discord-management-bot/test/receiver"
)

func TestDeliveryRoundTrip(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	gatewayPublic, gatewayKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	workerPublic, workerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public := httptest.NewUnstartedServer(nil)
	publicURL := "http://" + public.Listener.Addr().String()
	service, err := app.New(db, app.Config{Key: gatewayKey, GuildID: "test-guild", ResultURL: publicURL + "/results"})
	if err != nil {
		t.Fatal(err)
	}
	public.Config.Handler = service.PublicHandler()
	public.Start()
	t.Cleanup(public.Close)
	admin := httptest.NewServer(service.ManagementHandler())
	t.Cleanup(admin.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	api := func(t *testing.T, method, path string, body any) map[string]any {
		t.Helper()
		var input io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			input = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, admin.URL+path, input)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			b, _ := io.ReadAll(response.Body)
			t.Fatalf("%s %s: %d %s", method, path, response.StatusCode, b)
		}
		var result map[string]any
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	receiverServer := httptest.NewUnstartedServer(nil)
	t.Cleanup(receiverServer.Close)
	receiverURL := "http://" + receiverServer.Listener.Addr().String()
	destination := api(t, "POST", "/destinations", map[string]any{
		"name": "round-trip-test", "url": receiverURL + "/events", "result_public_key": protocol.EncodePublicKey(workerPublic),
	})
	receiverServer.Config.Handler = receiver.New(t, workerKey, gatewayPublic, publicURL+"/results", destination["id"].(string))
	receiverServer.Start()
	trigger := api(t, "POST", "/triggers", map[string]any{
		"name": "all-messages", "destination_id": destination["id"], "condition": `event_type == "MESSAGE_CREATE"`,
	})
	for _, status := range []string{"success", "partial_success", "failure", "skipped"} {
		t.Run(status, func(t *testing.T) {
			queued := api(t, "POST", "/tests/events", map[string]any{
				"event_type": "MESSAGE_CREATE", "data": map[string]any{"test_status": status},
				"trigger_ids": []any{trigger["id"]},
			})
			id := queued["delivery_ids"].([]any)[0].(string)
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				items := api(t, "GET", "/deliveries", nil)["items"].([]any)
				for _, entry := range items {
					item := entry.(map[string]any)
					if item["id"] == id && item["state"] == "finished" {
						if item["result_status"] != status {
							t.Fatalf("unexpected summary status: %v", item["result_status"])
						}
						if _, ok := item["payload"]; ok {
							t.Fatal("delivery list includes event payload")
						}
						if _, ok := item["result"]; ok {
							t.Fatal("delivery list includes full worker result")
						}
						result := api(t, "GET", "/deliveries/"+id, nil)
						if result["result"].(map[string]any)["status"] != status {
							t.Fatalf("unexpected outcome: %v", result["result"])
						}
						return
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("signed result notification did not finish")
		})
	}
}
