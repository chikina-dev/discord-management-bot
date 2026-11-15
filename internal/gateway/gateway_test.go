package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/store"
	"github.com/coder/websocket"
)

func TestReconnectResumesDurableSequence(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var connections, resumes, serverErrors atomic.Int64
	const endpoint = "wss://gateway.discord.gg"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErrors.Add(1)
			return
		}
		defer c.CloseNow()
		n := connections.Add(1)
		writePacket(ctx, c, 10, map[string]any{"heartbeat_interval": 50})
		auth, err := readPacket(ctx, c)
		if err != nil {
			serverErrors.Add(1)
			return
		}
		var fields map[string]any
		json.Unmarshal(auth.Data, &fields)
		if n == 1 {
			if auth.Op != 2 {
				serverErrors.Add(1)
				return
			}
			body, _ := json.Marshal(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{"session_id": "session", "resume_gateway_url": endpoint, "user": map[string]any{"id": "bot", "username": "fixture"}}})
			c.Write(ctx, websocket.MessageText, body)
			body = []byte(`{"op":0,"s":2,"t":"MESSAGE_CREATE","d":{"id":"first","guild_id":"guild"}}`)
			c.Write(ctx, websocket.MessageText, body)
		} else {
			if auth.Op != 6 || fields["session_id"] != "session" || fields["seq"] != float64(2) {
				serverErrors.Add(1)
				return
			}
			resumes.Add(1)
			c.Write(ctx, websocket.MessageText, []byte(`{"op":0,"s":3,"t":"RESUMED","d":{}}`))
			c.Write(ctx, websocket.MessageText, []byte(`{"op":0,"s":4,"t":"MESSAGE_CREATE","d":{"id":"second","guild_id":"guild"}}`))
		}
		for {
			p, err := readPacket(ctx, c)
			if err != nil {
				return
			}
			if p.Op == 1 {
				writePacket(ctx, c, 11, nil)
				if n == 1 {
					writePacket(ctx, c, 7, nil)
					return
				}
			}
		}
	})
	server := httptest.NewServer(discordFixture(handler))
	defer server.Close()
	g := New(Config{Token: "fixture-token", Store: s, Client: fixtureClient(server)})
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(6 * time.Second)
	for {
		state, err := s.GatewayState(ctx)
		if err == nil && state.Sequence >= 4 && g.Status().Authenticated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resume failed: %#v %v status=%#v server-errors=%d", state, err, g.Status(), serverErrors.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	stats, err := s.Stats(ctx, time.Now())
	if err != nil || stats.QueuedEvents != 2 {
		t.Fatalf("lost dispatch: %#v %v", stats, err)
	}
	if resumes.Load() != 1 || serverErrors.Load() != 0 {
		t.Fatalf("resume=%d server-errors=%d", resumes.Load(), serverErrors.Load())
	}
}
func TestMissingHeartbeatAckReconnects(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var connections atomic.Int64
	const endpoint = "wss://gateway.discord.gg"
	server := httptest.NewServer(discordFixture(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		n := connections.Add(1)
		writePacket(ctx, c, 10, map[string]any{"heartbeat_interval": 40})
		_, err = readPacket(ctx, c)
		if err != nil {
			return
		}
		if n == 1 {
			b, _ := json.Marshal(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{"session_id": "s", "resume_gateway_url": endpoint}})
			c.Write(ctx, websocket.MessageText, b)
		} else {
			c.Write(ctx, websocket.MessageText, []byte(`{"op":0,"s":2,"t":"RESUMED","d":{}}`))
		}
		for {
			p, err := readPacket(ctx, c)
			if err != nil {
				return
			}
			if p.Op == 1 && n > 1 {
				writePacket(ctx, c, 11, nil)
			}
		}
	})))
	defer server.Close()
	g := New(Config{Token: "fixture", Store: s, Client: fixtureClient(server)})
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for connections.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("unacknowledged connection remained active")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestProductionGatewayURLValidation(t *testing.T) {
	for _, raw := range []string{"ws://gateway.discord.gg", "wss://evil.example", "wss://gateway.discord.gg@evil.example", "wss://gateway.discord.gg:444", "https://gateway.discord.gg"} {
		if _, err := validURL(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"wss://gateway.discord.gg", "wss://gateway-us-east1-b.discord.gg"} {
		if _, err := validURL(raw); err != nil {
			t.Fatal(err)
		}
	}
}

// discordFixture keeps the production discovery and URL validation paths active.
func discordFixture(gateway http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v10/gateway/bot" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"url":"wss://gateway.discord.gg","session_start_limit":{"remaining":10,"reset_after":0}}`))
			return
		}
		gateway.ServeHTTP(w, r)
	})
}

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureClient(server *httptest.Server) *http.Client {
	target, _ := url.Parse(server.URL)
	return &http.Client{Timeout: 15 * time.Second, Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "discord.com" && r.URL.Hostname() != "gateway.discord.gg" {
			return nil, fmt.Errorf("unexpected fixture request to %s", r.URL.Host)
		}
		request := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = target.Scheme, target.Host
		request.URL, request.Host = &u, target.Host
		return http.DefaultTransport.RoundTrip(request)
	})}
}

func TestDiscoveryAcceptsZeroResetForUnusedBot(t *testing.T) {
	for _, remaining := range []int{1000, 1, 0} {
		t.Run(fmt.Sprint(remaining), func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"url":"wss://gateway.discord.gg","session_start_limit":{"remaining":%d,"reset_after":0}}`, remaining)
			}))
			defer server.Close()
			controller := New(Config{Token: "fixture-token", Store: db, Client: fixtureClient(server)})
			state := store.GatewayState{}
			before := time.Now()
			target, err := controller.discover(t.Context(), &state)
			if remaining > 0 {
				if err != nil || target != "wss://gateway.discord.gg" {
					t.Fatalf("valid zero reset rejected: %s %v", target, err)
				}
			} else if err == nil {
				t.Fatal("exhausted quota allowed identification")
			}
			if state.IdentifyAfter < before.Add(5*time.Second).UnixMilli() {
				t.Fatal("zero reset removed the minimum identify reservation")
			}
		})
	}
}

func TestTransportFailureStatusRedactsToken(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const token = "fixture-secret-token"
	controller := New(Config{Token: token, Store: db, Client: &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("transport failed with credential %s", token)
	})}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); controller.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		message := controller.Status().LastError
		if message != "" {
			if strings.Contains(message, token) || !strings.Contains(message, "[redacted]") {
				t.Fatalf("unsafe diagnostic: %s", message)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no diagnostic recorded")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}
