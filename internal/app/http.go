package app

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/rules"
	"github.com/chikina-dev/discord-management-bot/internal/store"
	"github.com/mattn/go-sqlite3"
)

const maxBodyBytes = 1 << 20

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if value != nil {
		data, err := protocol.JSON(value)
		if err == nil {
			_, _ = w.Write(data)
		}
	}
}
func problem(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func databaseError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		problem(w, 404, "not found")
		return
	}
	var e sqlite3.Error
	if errors.As(err, &e) && e.Code == sqlite3.ErrConstraint {
		problem(w, 409, "name already exists or referenced destination is missing")
		return
	}
	problem(w, 500, "database operation failed")
}
func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		problem(w, 415, "Content-Type must be application/json")
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	d.DisallowUnknownFields()
	if err = d.Decode(target); err != nil {
		problem(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		problem(w, 400, "expected exactly one JSON value")
		return false
	}
	return true
}

type destinationRequest struct {
	Name            string `json:"name"`
	URL             string `json:"url"`
	ResultPublicKey string `json:"result_public_key"`
}
type triggerRequest struct {
	DestinationID string `json:"destination_id"`
	Name          string `json:"name"`
	Condition     string `json:"condition"`
}
type evaluationRequest struct {
	Condition string          `json:"condition"`
	EventType string          `json:"event_type"`
	Data      json.RawMessage `json:"data"`
	Context   map[string]any  `json:"context"`
}
type testEventRequest struct {
	EventType  string          `json:"event_type"`
	Data       json.RawMessage `json:"data"`
	Context    map[string]any  `json:"context"`
	TriggerIDs []string        `json:"trigger_ids"`
}

func (a *App) ManagementHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]any{"ok": true}) })
	m.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		stat, err := a.Store.Stats(r.Context(), time.Now())
		if err != nil {
			databaseError(w, err)
			return
		}
		body := map[string]any{"storage": stat, "guild_id": a.cfg.GuildID, "guild_name": a.cfg.GuildName, "result_url": a.cfg.ResultURL}
		if a.cfg.Status != nil {
			body["gateway"] = a.cfg.Status()
		}
		respond(w, 200, body)
	})
	m.HandleFunc("GET /signing-key", func(w http.ResponseWriter, r *http.Request) {
		pub := a.cfg.Key.Public().(ed25519.PublicKey)
		respond(w, 200, map[string]string{"key_id": protocol.KeyID(pub), "public_key": protocol.EncodePublicKey(pub)})
	})
	m.HandleFunc("GET /capabilities", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"inputs": []string{"event_type", "data", "context"}, "logical": []string{"&&", "||", "!"}, "comparison": []string{"==", "!=", "===", "!==", "<", "<=", ">", ">="}, "methods": []string{"some", "every", "includes", "startsWith", "endsWith", "toLowerCase", "toUpperCase", "matches"}, "max_condition_bytes": rules.MaxSourceBytes, "max_condition_nodes": rules.MaxNodes, "result_statuses": []protocol.ResultStatus{protocol.Success, protocol.PartialSuccess, protocol.Failure, protocol.Skipped}})
	})
	m.HandleFunc("POST /gateway/reconnect", func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.Reconnect == nil {
			problem(w, 503, "gateway is not running")
			return
		}
		a.cfg.Reconnect()
		respond(w, 202, map[string]bool{"requested": true})
	})
	m.HandleFunc("GET /destinations", func(w http.ResponseWriter, r *http.Request) {
		items, err := a.Store.Destinations(r.Context())
		if err != nil {
			databaseError(w, err)
			return
		}
		respond(w, 200, map[string]any{"items": items})
	})
	m.HandleFunc("GET /destinations/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, err := a.Store.Destination(r.Context(), r.PathValue("id"))
		if err != nil {
			databaseError(w, err)
			return
		}
		respond(w, 200, d)
	})
	putDestination := func(create bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var in destinationRequest
			if !readJSON(w, r, &in) {
				return
			}
			d := store.Destination{Name: in.Name, URL: in.URL, ResultPublicKey: in.ResultPublicKey}
			if !create {
				d.ID = r.PathValue("id")
			}
			if err := store.ValidateDestination(d); err != nil {
				problem(w, 400, err.Error())
				return
			}
			saved, err := a.putDestination(r.Context(), d, create)
			if err != nil {
				databaseError(w, err)
				return
			}
			status := 200
			if create {
				status = 201
			}
			respond(w, status, saved)
		}
	}
	m.HandleFunc("POST /destinations", putDestination(true))
	m.HandleFunc("PUT /destinations/{id}", putDestination(false))
	m.HandleFunc("DELETE /destinations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := a.deleteDestination(r.Context(), r.PathValue("id")); err != nil {
			databaseError(w, err)
			return
		}
		respond(w, 204, nil)
	})
	m.HandleFunc("GET /triggers", func(w http.ResponseWriter, r *http.Request) {
		items, err := a.Store.Triggers(r.Context())
		if err != nil {
			databaseError(w, err)
			return
		}
		if id := r.URL.Query().Get("destination_id"); id != "" {
			filtered := []store.Trigger{}
			for _, item := range items {
				if item.DestinationID == id {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		}
		respond(w, 200, map[string]any{"items": items})
	})
	m.HandleFunc("GET /triggers/{id}", func(w http.ResponseWriter, r *http.Request) {
		item, err := a.Store.Trigger(r.Context(), r.PathValue("id"))
		if err != nil {
			databaseError(w, err)
			return
		}
		respond(w, 200, item)
	})
	putTrigger := func(create bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var in triggerRequest
			if !readJSON(w, r, &in) {
				return
			}
			if in.Name == "" || in.DestinationID == "" {
				problem(w, 400, "name and destination_id are required")
				return
			}
			item := store.Trigger{Name: in.Name, DestinationID: in.DestinationID, Condition: in.Condition}
			if !create {
				item.ID = r.PathValue("id")
			}
			saved, err := a.putTrigger(r.Context(), item, create)
			if err != nil {
				var syntax *rules.SyntaxError
				switch {
				case errors.As(err, &syntax):
					problem(w, 400, err.Error())
				case errors.Is(err, errDestinationMissing):
					problem(w, 409, err.Error())
				default:
					databaseError(w, err)
				}
				return
			}
			status := 200
			if create {
				status = 201
			}
			respond(w, status, saved)
		}
	}
	m.HandleFunc("POST /triggers", putTrigger(true))
	m.HandleFunc("PUT /triggers/{id}", putTrigger(false))
	m.HandleFunc("DELETE /triggers/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := a.deleteTrigger(r.Context(), r.PathValue("id")); err != nil {
			databaseError(w, err)
			return
		}
		respond(w, 204, nil)
	})
	m.HandleFunc("POST /rules/evaluate", func(w http.ResponseWriter, r *http.Request) {
		var in evaluationRequest
		if !readJSON(w, r, &in) {
			return
		}
		program, err := rules.Compile(in.Condition)
		if err != nil {
			problem(w, 400, err.Error())
			return
		}
		input, err := rules.DecodeInput(in.EventType, in.Data, in.Context)
		if err != nil {
			problem(w, 400, "invalid event data")
			return
		}
		result, err := program.Evaluate(input)
		if err != nil {
			problem(w, 422, err.Error())
			return
		}
		body := map[string]any{"match": result.Match, "uses_attachments": result.UsesAttachments, "attachment_indices": result.AttachmentIndices}
		if result.UsesAttachments {
			selected := []any{}
			if data, ok := input.Data.(map[string]any); ok {
				if files, ok := data["attachments"].([]any); ok {
					for _, i := range result.AttachmentIndices {
						selected = append(selected, files[i])
					}
				}
			}
			body["attachments"] = selected
		}
		respond(w, 200, body)
	})
	m.HandleFunc("POST /tests/events", func(w http.ResponseWriter, r *http.Request) {
		var in testEventRequest
		if !readJSON(w, r, &in) {
			return
		}
		if in.EventType == "" || len(in.Data) == 0 || len(in.TriggerIDs) == 0 || len(in.TriggerIDs) > 1000 {
			problem(w, 400, "event_type, data and explicit trigger_ids are required")
			return
		}
		var obj map[string]any
		if err := json.Unmarshal(in.Data, &obj); err != nil || obj == nil {
			problem(w, 400, "test data must be a JSON object")
			return
		}
		a.catalogMu.RLock()
		defer a.catalogMu.RUnlock()
		for _, id := range in.TriggerIDs {
			if _, err := a.Store.Trigger(r.Context(), id); err != nil {
				databaseError(w, err)
				return
			}
		}
		ev, err := a.Store.AddTestEvent(r.Context(), in.EventType, in.Data, in.Context, in.TriggerIDs...)
		if err != nil {
			databaseError(w, err)
			return
		}
		a.WakeEvents()
		respond(w, 202, map[string]any{"event_id": ev.ID, "delivery_ids": func() []string {
			out := []string{}
			for _, id := range in.TriggerIDs {
				out = append(out, store.DeliveryID(ev.ID, id))
			}
			return out
		}()})
	})
	m.HandleFunc("GET /deliveries", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		limit, _ := strconv.Atoi(query.Get("limit"))
		items, err := a.Store.Deliveries(r.Context(), query.Get("destination_id"), query.Get("state"), query.Get("before"), limit)
		if err != nil {
			databaseError(w, err)
			return
		}
		next := ""
		if len(items) > 0 {
			next = items[len(items)-1].ID
		}
		respond(w, 200, map[string]any{"items": items, "next_before": next})
	})
	m.HandleFunc("GET /deliveries/{id}", func(w http.ResponseWriter, r *http.Request) {
		item, err := a.Store.Delivery(r.Context(), r.PathValue("id"))
		if err != nil {
			databaseError(w, err)
			return
		}
		respond(w, 200, item)
	})
	m.HandleFunc("POST /deliveries/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Store.StopDelivery(r.Context(), r.PathValue("id")); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				databaseError(w, err)
			} else {
				problem(w, 409, err.Error())
			}
			return
		}
		respond(w, 204, nil)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsPrivate() && !ip.IsLoopback()) {
			problem(w, 403, "management API requires a private host")
			return
		}
		if r.Header.Get("Origin") != "" {
			problem(w, 403, "browser-origin management requests are not supported")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || kind != "application/json" {
				problem(w, 415, "Content-Type must be application/json")
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}

func (a *App) PublicHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /results", func(w http.ResponseWriter, r *http.Request) {
		select {
		case a.publicSlots <- struct{}{}:
			defer func() { <-a.publicSlots }()
		default:
			problem(w, 503, "busy")
			return
		}
		keyID := r.Header.Get(protocol.HeaderKeyID)
		key, err := a.Store.ResultKey(r.Context(), keyID)
		if err != nil {
			problem(w, 401, "invalid signature")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			problem(w, 413, "result body exceeds limit")
			return
		}
		if err = protocol.Verify(key, r.Method, protocol.RequestURL(r), body, r.Header, time.Now()); err != nil {
			problem(w, 401, "invalid signature")
			return
		}
		var result protocol.Result
		if err = json.Unmarshal(body, &result); err != nil {
			problem(w, 400, "invalid result JSON")
			return
		}
		if err = result.Validate(); err != nil {
			problem(w, 400, err.Error())
			return
		}
		created, err := a.Store.RecordResult(r.Context(), result, keyID, time.Now())
		if err != nil {
			switch {
			case errors.Is(err, store.ErrResultOwner):
				problem(w, 403, err.Error())
			case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotSent):
				problem(w, 409, err.Error())
			default:
				databaseError(w, err)
			}
			return
		}
		if created {
			metrics := map[string]float64{"WorkerResults": 1}
			switch result.Status {
			case protocol.Success:
				metrics["WorkerSuccess"] = 1
			case protocol.PartialSuccess:
				metrics["WorkerPartialSuccess"] = 1
			case protocol.Failure:
				metrics["WorkerFailure"] = 1
			case protocol.Skipped:
				metrics["WorkerSkipped"] = 1
			}
			start, _ := time.Parse(time.RFC3339Nano, result.Timestamps.WorkerAcceptedAt)
			finish, _ := time.Parse(time.RFC3339Nano, result.Timestamps.WorkerFinishedAt)
			metrics["WorkerMilliseconds"] = float64(finish.Sub(start).Milliseconds())
			a.cfg.Meter.Emit("worker_result", map[string]any{"delivery_id": result.DeliveryID, "destination_id": result.DestinationID, "status": result.Status}, metrics)
		}
		respond(w, 204, nil)
	})
	return m
}
