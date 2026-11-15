package receiver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

type fixtureDispatch struct {
	Sequence  int64
	EventType string
	Data      json.RawMessage
}
type gatewayFixture struct {
	mu       sync.Mutex
	conn     *websocket.Conn
	sequence int64
	history  []fixtureDispatch
}

func (g *gatewayFixture) write(ctx context.Context, c *websocket.Conn, op int, data any) error {
	b, err := json.Marshal(map[string]any{"op": op, "d": data})
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}
func (g *gatewayFixture) dispatch(ctx context.Context, eventType string, data json.RawMessage) error {
	g.sequence++
	event := fixtureDispatch{g.sequence, eventType, append(json.RawMessage{}, data...)}
	g.history = append(g.history, event)
	if len(g.history) > 5000 {
		g.history = append([]fixtureDispatch{}, g.history[len(g.history)-5000:]...)
	}
	return g.send(ctx, g.conn, event)
}
func (g *gatewayFixture) send(ctx context.Context, c *websocket.Conn, event fixtureDispatch) error {
	b, err := json.Marshal(map[string]any{"op": 0, "s": event.Sequence, "t": event.EventType, "d": event.Data})
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}
func addGatewayFixture(mux *http.ServeMux, ctx context.Context) {
	g := &gatewayFixture{}
	mux.HandleFunc("GET /gateway", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		if err = g.write(ctx, c, 10, map[string]any{"heartbeat_interval": 41250}); err != nil {
			return
		}
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		var auth struct {
			Op   int `json:"op"`
			Data struct {
				Sequence int64 `json:"seq"`
			} `json:"d"`
		}
		if json.Unmarshal(b, &auth) != nil {
			return
		}
		g.mu.Lock()
		g.conn = c
		if auth.Op == 6 {
			for _, event := range g.history {
				if event.Sequence > auth.Data.Sequence {
					if g.send(ctx, c, event) != nil {
						g.mu.Unlock()
						return
					}
				}
			}
			_ = g.dispatch(ctx, "RESUMED", json.RawMessage("{}"))
		} else {
			g.sequence = 0
			g.history = nil
			ready, _ := json.Marshal(map[string]any{"session_id": "fixture-session", "resume_gateway_url": "ws://" + r.Host + "/gateway", "user": map[string]any{"id": "fixture", "username": "benchmark"}})
			_ = g.dispatch(ctx, "READY", ready)
			_ = g.dispatch(ctx, "GUILD_CREATE", json.RawMessage(`{"id":"test-guild","name":"times_ちきな","channels":[{"id":"forum","type":15,"name":"fixture-forum"},{"id":"channel","type":11,"name":"fixture-thread","parent_id":"forum"}],"threads":[]}`))
		}
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			if g.conn == c {
				g.conn = nil
			}
			g.mu.Unlock()
		}()
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var p struct {
				Op int `json:"op"`
			}
			if json.Unmarshal(b, &p) != nil {
				return
			}
			if p.Op == 1 {
				if g.write(ctx, c, 11, nil) != nil {
					return
				}
			}
		}
	})
	mux.HandleFunc("POST /fixture/events", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			EventType string          `json:"event_type"`
			Data      json.RawMessage `json:"data"`
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(413)
			return
		}
		if json.Unmarshal(b, &in) != nil || in.EventType == "" || !json.Valid(in.Data) {
			w.WriteHeader(400)
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.conn == nil {
			w.WriteHeader(503)
			return
		}
		if g.dispatch(ctx, in.EventType, in.Data) != nil {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(202)
	})
}
