package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/store"
	"github.com/chikina-dev/discord-management-bot/internal/telemetry"
	"github.com/coder/websocket"
)

const DefaultIntents int64 = 1 | 512 | 32768

type Config struct {
	Token      string
	Intents    int64
	Store      *store.Store
	Meter      *telemetry.Meter
	Client     *http.Client
	OnDispatch func()
}
type Status struct {
	Expected           bool   `json:"expected"`
	Connected          bool   `json:"connected"`
	Authenticated      bool   `json:"authenticated"`
	Connecting         bool   `json:"connecting"`
	Fatal              bool   `json:"fatal"`
	LastHeartbeatAckAt string `json:"last_heartbeat_ack_at,omitempty"`
	BotID              string `json:"bot_id,omitempty"`
	BotName            string `json:"bot_name,omitempty"`
	Intents            int64  `json:"intents"`
	LastError          string `json:"last_error,omitempty"`
}
type Controller struct {
	cfg    Config
	mu     sync.Mutex
	status Status
	conn   *websocket.Conn
	reset  atomic.Bool
	wake   chan struct{}
}
type packet struct {
	Op       int             `json:"op"`
	Data     json.RawMessage `json:"d"`
	Sequence *int64          `json:"s"`
	Type     string          `json:"t"`
}
type reconnectError struct {
	invalidate bool
	reason     string
}

func (e *reconnectError) Error() string { return e.reason }

func New(cfg Config) *Controller {
	if cfg.Intents == 0 {
		cfg.Intents = DefaultIntents
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Controller{cfg: cfg, wake: make(chan struct{}, 1), status: Status{Expected: cfg.Token != "", Intents: cfg.Intents}}
}
func (g *Controller) Status() Status         { g.mu.Lock(); defer g.mu.Unlock(); return g.status }
func (g *Controller) SetOnDispatch(f func()) { g.cfg.OnDispatch = f }
func (g *Controller) Reconnect() {
	g.reset.Store(true)
	g.mu.Lock()
	if g.conn != nil {
		_ = g.conn.CloseNow()
	}
	g.mu.Unlock()
	select {
	case g.wake <- struct{}{}:
	default:
	}
}
func (g *Controller) update(f func(*Status)) {
	g.mu.Lock()
	f(&g.status)
	s := g.status
	g.mu.Unlock()
	connected := float64(0)
	if s.Connected && s.Authenticated {
		connected = 1
	}
	expected := float64(0)
	if s.Expected {
		expected = 1
	}
	g.cfg.Meter.Emit("gateway_status", map[string]any{"status": s}, map[string]float64{"GatewayConnected": connected, "GatewayExpected": expected})
}
func (g *Controller) wait(ctx context.Context, d time.Duration) bool {
	if d < 0 {
		d = 0
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-g.wake:
		return true
	case <-timer.C:
		return true
	}
}
func (g *Controller) Run(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
				g.update(func(*Status) {})
			}
		}
	}()
	failures := 0
	for ctx.Err() == nil {
		state, err := g.cfg.Store.GatewayState(ctx)
		if err != nil {
			g.update(func(s *Status) { s.LastError = "cannot load durable gateway state" })
			if !g.wait(ctx, time.Second) {
				return
			}
			continue
		}
		hash := sha256.Sum256([]byte(g.cfg.Token + ":" + strconv.FormatInt(g.cfg.Intents, 10)))
		fingerprint := hex.EncodeToString(hash[:])
		if state.Fingerprint != fingerprint {
			state = store.GatewayState{Fingerprint: fingerprint, IdentifyAfter: state.IdentifyAfter}
			_ = g.cfg.Store.SetGatewayState(ctx, state)
		}
		if g.reset.Swap(false) {
			state.Fatal = false
			state.ReconnectAfter = 0
			_ = g.cfg.Store.SetGatewayState(ctx, state)
		}
		if g.cfg.Token == "" {
			g.update(func(s *Status) { s.Expected = false; s.LastError = "receiver token is not configured" })
			if !g.wait(ctx, time.Minute) {
				return
			}
			continue
		}
		if state.Fatal {
			g.update(func(s *Status) { s.Fatal = true; s.Connected = false; s.Authenticated = false })
			if !g.wait(ctx, time.Minute) {
				return
			}
			continue
		}
		after := state.ReconnectAfter
		if state.SessionID == "" && state.IdentifyAfter > after {
			after = state.IdentifyAfter
		}
		if after > time.Now().UnixMilli() {
			if !g.wait(ctx, time.Until(time.UnixMilli(after))) {
				return
			}
			continue
		}
		g.update(func(s *Status) { s.Fatal = false; s.Connecting = true; s.LastError = "" })
		err = g.connect(ctx, &state, &failures)
		g.update(func(s *Status) { s.Connected = false; s.Authenticated = false; s.Connecting = false })
		if ctx.Err() != nil {
			return
		}
		if g.reset.Load() {
			continue
		}
		var directive *reconnectError
		if errors.As(err, &directive) && directive.invalidate {
			state.SessionID = ""
			state.ResumeURL = ""
			state.Sequence = 0
		}
		code := int(websocket.CloseStatus(err))
		switch code {
		case 1000, 1001, 4007, 4009:
			state.SessionID = ""
			state.ResumeURL = ""
			state.Sequence = 0
		case 4004, 4010, 4011, 4012, 4013, 4014:
			state.Fatal = true
			state.SessionID = ""
			state.ResumeURL = ""
			state.Sequence = 0
		}
		failures = min(failures+1, 6)
		delay := time.Duration(min(30, 1<<uint(failures-1)))*time.Second + time.Duration(rand.IntN(500))*time.Millisecond
		state.ReconnectAfter = time.Now().Add(delay).UnixMilli()
		if e := g.cfg.Store.SetGatewayState(ctx, state); e != nil {
			g.cfg.Meter.Emit("gateway_state_error", map[string]any{"error": e.Error()}, nil)
		}
		message := "connection ended"
		if code > 0 {
			message = fmt.Sprintf("Discord close %d", code)
		} else if directive != nil {
			message = directive.reason
		} else if err != nil {
			message = strings.ReplaceAll(err.Error(), g.cfg.Token, "[redacted]")
			if len(message) > 512 {
				message = message[:512]
			}
		}
		g.update(func(s *Status) { s.Fatal = state.Fatal; s.LastError = message })
	}
}
func validURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("invalid gateway URL")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "wss" || !(host == "gateway.discord.gg" || strings.HasSuffix(host, ".discord.gg")) || u.Port() != "" && u.Port() != "443" {
		return nil, errors.New("untrusted gateway URL")
	}
	q := u.Query()
	q.Set("v", "10")
	q.Set("encoding", "json")
	u.RawQuery = q.Encode()
	return u, nil
}
func (g *Controller) discover(ctx context.Context, state *store.GatewayState) (string, error) {
	request, err := http.NewRequestWithContext(ctx, "GET", "https://discord.com/api/v10/gateway/bot", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bot "+g.cfg.Token)
	request.Header.Set("User-Agent", "DiscordBot (https://github.com/chikina-dev/discord-management-bot, 1)")
	response, err := g.cfg.Client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		if response.StatusCode == 401 || response.StatusCode == 403 {
			state.Fatal = true
		}
		wait := 60.0
		if sec, err := strconv.ParseFloat(response.Header.Get("Retry-After"), 64); err == nil && sec > 0 {
			wait = sec
		}
		state.IdentifyAfter = time.Now().Add(time.Duration(wait * float64(time.Second))).UnixMilli()
		if err := g.cfg.Store.SetGatewayState(ctx, *state); err != nil {
			return "", err
		}
		return "", errors.New("gateway discovery rejected")
	}
	var data struct {
		URL   string `json:"url"`
		Limit struct {
			Remaining  *int    `json:"remaining"`
			ResetAfter float64 `json:"reset_after"`
		} `json:"session_start_limit"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&data); err != nil {
		return "", err
	}
	if data.Limit.Remaining == nil || *data.Limit.Remaining < 0 || data.Limit.ResetAfter < 0 {
		return "", errors.New("invalid gateway discovery")
	}
	delay := 5 * time.Second
	if *data.Limit.Remaining <= 1 {
		delay = max(delay, time.Duration(data.Limit.ResetAfter*float64(time.Millisecond)))
	}
	state.IdentifyAfter = time.Now().Add(delay).UnixMilli()
	if err = g.cfg.Store.SetGatewayState(ctx, *state); err != nil {
		return "", err
	}
	if *data.Limit.Remaining <= 0 {
		return "", errors.New("gateway session start quota exhausted")
	}
	return data.URL, nil
}
func writePacket(ctx context.Context, c *websocket.Conn, op int, data any) error {
	body, err := json.Marshal(map[string]any{"op": op, "d": data})
	if err != nil {
		return err
	}
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(call, websocket.MessageText, body)
}
func readPacket(ctx context.Context, c *websocket.Conn) (packet, error) {
	_, body, err := c.Read(ctx)
	if err != nil {
		return packet{}, err
	}
	var p packet
	if err = json.Unmarshal(body, &p); err != nil {
		return p, errors.New("invalid gateway JSON")
	}
	return p, nil
}
func (g *Controller) connect(ctx context.Context, state *store.GatewayState, failures *int) error {
	raw := state.ResumeURL
	if state.SessionID == "" {
		var err error
		raw, err = g.discover(ctx, state)
		if err != nil {
			return err
		}
	}
	u, err := validURL(raw)
	if err != nil {
		state.SessionID = ""
		state.ResumeURL = ""
		return err
	}
	upgrade, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(upgrade, u.String(), &websocket.DialOptions{HTTPClient: g.cfg.Client, CompressionMode: websocket.CompressionDisabled})
	cancel()
	if err != nil {
		return err
	}
	c.SetReadLimit(8 << 20)
	g.mu.Lock()
	g.conn = c
	g.mu.Unlock()
	defer func() { _ = c.CloseNow(); g.mu.Lock(); g.conn = nil; g.mu.Unlock() }()
	g.update(func(s *Status) { s.Connected = true; s.Connecting = false; s.Authenticated = false })
	connection, stop := context.WithCancel(ctx)
	defer stop()
	helloContext, helloCancel := context.WithTimeout(connection, 15*time.Second)
	hello, err := readPacket(helloContext, c)
	helloCancel()
	if err != nil {
		return err
	}
	if hello.Op != 10 {
		return errors.New("expected gateway Hello")
	}
	var h struct {
		Interval float64 `json:"heartbeat_interval"`
	}
	if err = json.Unmarshal(hello.Data, &h); err != nil || h.Interval < 10 || h.Interval > 300000 {
		return errors.New("invalid heartbeat interval")
	}
	interval := time.Duration(h.Interval * float64(time.Millisecond))
	var sequence atomic.Int64
	sequence.Store(state.Sequence)
	var pending atomic.Bool
	var sentAt atomic.Int64
	heartbeat := func(regular bool) error {
		now := time.Now()
		if regular && pending.Load() && now.UnixNano()-sentAt.Load() >= int64(interval) {
			return &reconnectError{reason: "heartbeat acknowledgement timed out"}
		}
		var data any
		if s := sequence.Load(); s > 0 {
			data = s
		}
		pending.Store(true)
		sentAt.Store(now.UnixNano())
		return writePacket(connection, c, 1, data)
	}
	if state.SessionID != "" {
		err = writePacket(connection, c, 6, map[string]any{"token": g.cfg.Token, "session_id": state.SessionID, "seq": state.Sequence})
	} else {
		err = writePacket(connection, c, 2, map[string]any{"token": g.cfg.Token, "intents": g.cfg.Intents, "properties": map[string]string{"os": "linux", "browser": "discord-management-bot", "device": "discord-management-bot"}, "compress": false, "shard": []int{0, 1}})
	}
	if err != nil {
		return err
	}
	heartbeatError := make(chan error, 1)
	go func() {
		timer := time.NewTimer(time.Duration(rand.Int64N(int64(interval))))
		defer timer.Stop()
		for {
			select {
			case <-connection.Done():
				return
			case <-timer.C:
			}
			if err := heartbeat(true); err != nil {
				heartbeatError <- err
				stop()
				_ = c.CloseNow()
				return
			}
			timer.Reset(interval)
		}
	}()
	for {
		p, err := readPacket(connection, c)
		if err != nil {
			select {
			case e := <-heartbeatError:
				return e
			default:
				return err
			}
		}
		switch p.Op {
		case 11:
			pending.Store(false)
			g.mu.Lock()
			g.status.LastHeartbeatAckAt = protocol.Time(time.Now())
			g.mu.Unlock()
		case 1:
			if err = heartbeat(false); err != nil {
				return err
			}
		case 7:
			return &reconnectError{reason: "Discord requested reconnect"}
		case 9:
			var resumable bool
			_ = json.Unmarshal(p.Data, &resumable)
			return &reconnectError{invalidate: !resumable, reason: "Discord invalidated session"}
		case 0:
			if p.Sequence == nil || p.Type == "" {
				return errors.New("dispatch missing sequence or type")
			}
			next := *state
			if p.Type == "READY" {
				var ready struct {
					SessionID string `json:"session_id"`
					ResumeURL string `json:"resume_gateway_url"`
					User      struct {
						ID       string `json:"id"`
						Username string `json:"username"`
					} `json:"user"`
				}
				if err = json.Unmarshal(p.Data, &ready); err != nil || ready.SessionID == "" {
					return errors.New("invalid Ready event")
				}
				if _, err = validURL(ready.ResumeURL); err != nil {
					return err
				}
				next.SessionID = ready.SessionID
				next.ResumeURL = ready.ResumeURL
				g.mu.Lock()
				g.status.BotID = ready.User.ID
				g.status.BotName = ready.User.Username
				g.mu.Unlock()
			}
			if next.SessionID == "" {
				return errors.New("dispatch before session established")
			}
			if next.SessionID == state.SessionID && *p.Sequence <= state.Sequence {
				continue
			}
			next.Sequence = *p.Sequence
			if err = g.cfg.Store.SaveDispatch(connection, next, p.Type, p.Data, time.Now()); err != nil {
				return err
			}
			*state = next
			sequence.Store(state.Sequence)
			if p.Type == "READY" || p.Type == "RESUMED" {
				*failures = 0
				g.update(func(s *Status) { s.Authenticated = true; s.LastError = "" })
			}
			if g.cfg.OnDispatch != nil {
				g.cfg.OnDispatch()
			}
		}
	}
}
