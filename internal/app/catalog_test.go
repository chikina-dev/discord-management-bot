package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/store"
)

type metadataTransport func(*http.Request) (*http.Response, error)

func (f metadataTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCatalogChangesDoNotWaitForChannelLookup(t *testing.T) {
	for _, action := range []string{"delete_trigger", "update_trigger", "update_destination"} {
		t.Run(action, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			publicKey, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			lookupStarted, releaseLookup := make(chan struct{}), make(chan struct{})
			var started, released sync.Once
			unblock := func() { released.Do(func() { close(releaseLookup) }) }
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			service, err := New(db, Config{
				Key: key, GuildID: "guild", BotToken: "fixture",
				Client: &http.Client{Transport: metadataTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.String() != "https://discord.com/api/v10/channels/channel" {
						return nil, fmt.Errorf("unexpected metadata request: %s", r.URL)
					}
					started.Do(func() { close(lookupStarted) })
					select {
					case <-releaseLookup:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"channel","guild_id":"guild","parent_id":"forum"}`))}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			admin := httptest.NewServer(service.ManagementHandler())
			defer admin.Close()
			defer unblock()
			var destination store.Destination
			if err := json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/destinations", destinationRequest{
				Name: "worker", URL: "https://old.example/events", ResultPublicKey: protocol.EncodePublicKey(publicKey),
			}, 201), &destination); err != nil {
				t.Fatal(err)
			}
			var trigger store.Trigger
			if err := json.Unmarshal(request(t, admin.Client(), "POST", admin.URL+"/triggers", triggerRequest{
				Name: "forum", DestinationID: destination.ID, Condition: `context.channel.parent_id == "forum"`,
			}, 201), &trigger); err != nil {
				t.Fatal(err)
			}
			if err := db.SaveDispatch(ctx, store.GatewayState{SessionID: "discord-session", Sequence: 1}, "MESSAGE_CREATE", json.RawMessage(`{"id":"message","guild_id":"guild","channel_id":"channel"}`), time.Now()); err != nil {
				t.Fatal(err)
			}
			event, err := db.NextEvent(ctx, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			processed := make(chan error, 1)
			go func() { processed <- service.processEvent(ctx, event) }()
			select {
			case <-lookupStarted:
			case <-time.After(time.Second):
				t.Fatal("channel lookup did not start")
			}

			method, target, status := "DELETE", "/triggers/"+trigger.ID, 204
			var body any
			switch action {
			case "update_trigger":
				method, status = "PUT", 200
				body = triggerRequest{Name: trigger.Name, DestinationID: destination.ID, Condition: "false"}
			case "update_destination":
				method, target, status = "PUT", "/destinations/"+destination.ID, 200
				body = destinationRequest{Name: destination.Name, URL: "https://updated.example/events", ResultPublicKey: destination.ResultPublicKey}
			}
			payload, _ := protocol.JSON(body)
			req, err := http.NewRequestWithContext(ctx, method, admin.URL+target, bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			mutated := make(chan error, 1)
			go func() {
				response, err := admin.Client().Do(req)
				if err == nil {
					response.Body.Close()
					if response.StatusCode != status {
						err = fmt.Errorf("mutation returned HTTP %d", response.StatusCode)
					}
				}
				mutated <- err
			}()
			select {
			case err := <-mutated:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("catalogue mutation waited for the network lookup")
			}
			unblock()
			select {
			case err := <-processed:
				if !errors.Is(err, errCatalogChanged) {
					t.Fatalf("stale catalogue was not retried: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("event processing did not return")
			}
			jobs, err := db.Deliveries(ctx, destination.ID, "", "", 100)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("stale processing created deliveries: %+v, %v", jobs, err)
			}
			pending, err := db.NextEvent(ctx, time.Now())
			if err != nil || pending.ID != event.ID {
				t.Fatalf("event was not retained for retry: %+v, %v", pending, err)
			}
			if err := service.processEvent(ctx, pending); err != nil {
				t.Fatal(err)
			}
			jobs, err = db.Deliveries(ctx, destination.ID, "", "", 100)
			if err != nil {
				t.Fatal(err)
			}
			if action == "update_destination" {
				if len(jobs) != 1 || jobs[0].URL != "https://updated.example/events" {
					t.Fatalf("retried event did not use the updated destination: %+v", jobs)
				}
			} else if len(jobs) != 0 {
				t.Fatalf("retried event used the removed or changed trigger: %+v", jobs)
			}
			if _, err := db.NextEvent(ctx, time.Now()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("retried event did not finish: %v", err)
			}
		})
	}
}
