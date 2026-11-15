package app

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/rules"
	"github.com/chikina-dev/discord-management-bot/internal/store"
	"github.com/chikina-dev/discord-management-bot/internal/telemetry"
)

type Config struct {
	GuildID   string
	GuildName string
	BotToken  string
	ResultURL string
	Key       ed25519.PrivateKey
	Client    *http.Client
	Meter     *telemetry.Meter
	Status    func() any
	Reconnect func()
}
type binding struct {
	trigger     store.Trigger
	destination store.Destination
	program     *rules.Program
}
type App struct {
	Store             *store.Store
	cfg               Config
	catalogMu         sync.RWMutex
	bindings          []binding
	catalogGeneration uint64
	usesContext       bool
	publicSlots       chan struct{}
	eventWake         chan struct{}
	deliveryWake      chan struct{}
}

var errCatalogChanged = errors.New("catalogue changed while loading event context")

func New(s *store.Store, cfg Config) (*App, error) {
	if len(cfg.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("valid Ed25519 key is required")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	client := *cfg.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout == 0 {
		client.Timeout = 10 * time.Second
	}
	cfg.Client = &client
	a := &App{Store: s, cfg: cfg, publicSlots: make(chan struct{}, 8), eventWake: make(chan struct{}, 1), deliveryWake: make(chan struct{}, 1)}
	if err := a.loadCatalog(context.Background()); err != nil {
		return nil, err
	}
	return a, nil
}
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, fn := range []func(context.Context){a.processEvents, a.sendDeliveries, a.observe} {
		wg.Add(1)
		go func(f func(context.Context)) { defer wg.Done(); f(ctx) }(fn)
	}
	wg.Wait()
}
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (a *App) WakeEvents() {
	select {
	case a.eventWake <- struct{}{}:
	default:
	}
}
func (a *App) wakeDeliveries() {
	select {
	case a.deliveryWake <- struct{}{}:
	default:
	}
}
func waitSignal(ctx context.Context, wake <-chan struct{}) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}
func (a *App) processEvents(ctx context.Context) {
	for ctx.Err() == nil {
		event, err := a.Store.NextEvent(ctx, time.Now())
		if errors.Is(err, store.ErrNotFound) {
			if !waitSignal(ctx, a.eventWake) {
				return
			}
			continue
		}
		if err != nil {
			a.cfg.Meter.Emit("event_read_error", map[string]any{"error": err.Error()}, nil)
			sleep(ctx, time.Second)
			continue
		}
		if err = a.processEvent(ctx, event); err != nil {
			if errors.Is(err, errCatalogChanged) {
				continue
			}
			a.cfg.Meter.Emit("event_processing_error", map[string]any{"event_id": event.ID, "error": err.Error()}, nil)
			_ = a.Store.RetryEvent(ctx, event.ID, err)
			sleep(ctx, 100*time.Millisecond)
		}
	}
}
func (a *App) processEvent(ctx context.Context, event store.Event) error {
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return err
	}
	isTest := strings.HasPrefix(event.SessionID, "test:") || event.SessionID == event.ID
	guildID, _ := data["guild_id"].(string)
	if strings.HasPrefix(event.EventType, "GUILD_") && guildID == "" {
		guildID, _ = data["id"].(string)
	}
	if !isTest && guildID != "" && guildID != a.cfg.GuildID {
		return a.Store.CommitEvent(ctx, event.ID, nil)
	}
	if !isTest && a.cfg.GuildID == "" {
		return errors.New("target guild is not configured")
	}
	a.catalogMu.RLock()
	generation, usesContext := a.catalogGeneration, a.usesContext
	a.catalogMu.RUnlock()
	proposedContext, channels, err := a.prepareEventContext(ctx, event, data, isTest, usesContext)
	if err != nil {
		return err
	}
	a.catalogMu.RLock()
	defer a.catalogMu.RUnlock()
	if generation != a.catalogGeneration {
		return errCatalogChanged
	}
	captured, err := a.Store.CaptureEventContext(ctx, event.ID, a.cfg.GuildID, channels, proposedContext)
	if err != nil {
		return err
	}
	inputContext := captured.Data
	if !isTest && guildID == "" {
		channel, _ := inputContext["channel"].(map[string]any)
		if channel == nil || channel["guild_id"] != a.cfg.GuildID {
			return a.Store.CommitEvent(ctx, event.ID, nil)
		}
	}
	input := rules.Input{EventType: event.EventType, Data: data, Context: inputContext}
	allowed := map[string]bool{}
	for _, id := range event.TriggerIDs {
		allowed[id] = true
	}
	var jobs []store.NewDelivery
	for _, b := range a.bindings {
		if isTest && !allowed[b.trigger.ID] {
			continue
		}
		result, err := b.program.Evaluate(input)
		if err != nil {
			return fmt.Errorf("trigger %s: %w", b.trigger.ID, err)
		}
		if !result.Match {
			continue
		}
		env := protocol.Envelope{DeliveryID: store.DeliveryID(event.ID, b.trigger.ID), EventID: event.ID, TriggerID: b.trigger.ID, DestinationID: b.destination.ID, EventType: event.EventType, Timestamps: protocol.Timestamps{EC2ReceivedAt: event.ReceivedAt}, Data: event.Data, Context: inputContext}
		if result.UsesAttachments {
			files := []json.RawMessage{}
			if rawFiles, ok := data["attachments"].([]any); ok {
				for _, i := range result.AttachmentIndices {
					if i < len(rawFiles) {
						raw, err := protocol.JSON(rawFiles[i])
						if err != nil {
							return err
						}
						files = append(files, raw)
					}
				}
			}
			env.Attachments = &files
		}
		jobs = append(jobs, store.NewDelivery{Envelope: env, Trigger: b.trigger, Destination: b.destination})
	}
	if err = a.Store.CommitEvent(ctx, event.ID, jobs); err != nil {
		return err
	}
	if len(jobs) > 0 {
		a.wakeDeliveries()
	}
	a.cfg.Meter.Emit("event_processed", map[string]any{"event_id": event.ID, "event_type": event.EventType, "matched": len(jobs)}, map[string]float64{"EventsProcessed": 1, "TriggerMatches": float64(len(jobs))})
	return nil
}
func (a *App) prepareEventContext(ctx context.Context, event store.Event, data map[string]any, isTest, usesContext bool) (store.EventContextSnapshot, []map[string]any, error) {
	saved, err := a.Store.EventContext(ctx, event.ID)
	captured := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.EventContextSnapshot{}, nil, err
	}
	if captured && (saved.Complete || !usesContext) {
		return saved, nil, nil
	}
	if isTest {
		out := map[string]any{}
		if len(event.SuppliedContext) > 0 {
			if err := json.Unmarshal(event.SuppliedContext, &out); err != nil {
				return store.EventContextSnapshot{}, nil, err
			}
		}
		if out == nil {
			out = map[string]any{}
		}
		out["source"] = "test"
		return store.EventContextSnapshot{Data: out, Complete: true}, nil, nil
	}
	out := map[string]any{"guild": map[string]any{"id": a.cfg.GuildID, "name": a.cfg.GuildName}}
	var channels []map[string]any
	cid, _ := data["channel_id"].(string)
	if captured {
		out = saved.Data
		if channel, ok := out["channel"].(map[string]any); ok {
			cid, _ = channel["id"].(string)
		}
	} else {
		if event.EventType == "GUILD_CREATE" || event.EventType == "THREAD_LIST_SYNC" {
			for _, field := range []string{"channels", "threads"} {
				if list, ok := data[field].([]any); ok {
					for _, value := range list {
						if channel, ok := value.(map[string]any); ok {
							channels = append(channels, channel)
						}
					}
				}
			}
		}
		if strings.HasPrefix(event.EventType, "CHANNEL_") || event.EventType == "THREAD_CREATE" || event.EventType == "THREAD_UPDATE" || event.EventType == "THREAD_DELETE" {
			if id, ok := data["id"].(string); ok {
				cid = id
				channels = append(channels, data)
			}
		}
	}
	if cid == "" {
		return store.EventContextSnapshot{Data: out, Complete: true}, channels, nil
	}
	channel, err := a.Store.Channel(ctx, cid)
	known := err == nil
	if errors.Is(err, store.ErrNotFound) {
		channel = map[string]any{"id": cid}
	} else if err != nil {
		return store.EventContextSnapshot{}, nil, err
	}
	for _, observation := range channels {
		if observation["id"] == cid {
			for key, value := range observation {
				if key != "newly_created" {
					channel[key] = value
				}
			}
			channel["guild_id"] = a.cfg.GuildID
			known = true
		}
	}
	if !known && usesContext && a.cfg.BotToken != "" {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		u := "https://discord.com/api/v10/channels/" + url.PathEscape(cid)
		request, err := http.NewRequestWithContext(callCtx, http.MethodGet, u, nil)
		if err != nil {
			return store.EventContextSnapshot{}, nil, err
		}
		request.Header.Set("Authorization", "Bot "+a.cfg.BotToken)
		response, err := a.cfg.Client.Do(request)
		if err != nil {
			return store.EventContextSnapshot{}, nil, errors.New("channel metadata request failed")
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return store.EventContextSnapshot{}, nil, fmt.Errorf("channel metadata HTTP %d", response.StatusCode)
		}
		if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&channel); err != nil {
			return store.EventContextSnapshot{}, nil, err
		}
		if channel["guild_id"] != a.cfg.GuildID {
			return store.EventContextSnapshot{}, nil, errors.New("channel does not belong to target guild")
		}
		known = true
		if !captured {
			channels = append(channels, channel)
		}
	}
	out["channel"] = channel
	return store.EventContextSnapshot{Data: out, Complete: known}, channels, nil
}
func retryDelay(attempt int) time.Duration {
	seconds := min(300, 1<<min(max(attempt-1, 0), 9))
	return time.Duration(float64(time.Second) * float64(seconds) * (0.8 + rand.Float64()*0.2))
}
func (a *App) sendDeliveries(ctx context.Context) {
	var wg sync.WaitGroup
	done := make(chan string, 4)
	active := map[string]bool{}
	defer wg.Wait()
	for ctx.Err() == nil {
		for {
			select {
			case id := <-done:
				delete(active, id)
			default:
				goto drained
			}
		}
	drained:
		ids, err := a.Store.ReadyDestinations(ctx, time.Now())
		if err != nil {
			a.cfg.Meter.Emit("delivery_queue_error", map[string]any{"error": err.Error()}, nil)
			sleep(ctx, time.Second)
			continue
		}
		for _, id := range ids {
			if len(active) >= 4 {
				break
			}
			if active[id] {
				continue
			}
			active[id] = true
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				a.sendOne(ctx, id)
				select {
				case done <- id:
				case <-ctx.Done():
				}
			}(id)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case id := <-done:
			delete(active, id)
		case <-a.deliveryWake:
		case <-timer.C:
		}
		timer.Stop()
	}
}
func (a *App) sendOne(ctx context.Context, destinationID string) {
	job, err := a.Store.PrepareDelivery(ctx, destinationID, time.Now())
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		a.cfg.Meter.Emit("delivery_prepare_error", map[string]any{"error": err.Error()}, nil)
		return
	}
	start := time.Now()
	u, err := url.Parse(job.URL)
	status := 0
	if err == nil {
		request, e := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(job.Payload)))
		err = e
		if err == nil {
			request.Header = protocol.Sign(a.cfg.Key, http.MethodPost, u, job.Payload, start)
			response, e := a.cfg.Client.Do(request)
			err = e
			if response != nil {
				status = response.StatusCode
				if status == 202 {
					var acceptance protocol.Acceptance
					err = json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&acceptance)
					response.Body.Close()
					if err == nil {
						err = a.Store.RecordAcceptance(ctx, job.ID, acceptance)
					}
					if err == nil {
						a.cfg.Meter.Emit("delivery_accepted", map[string]any{"delivery_id": job.ID, "destination_id": job.DestinationID, "attempt": job.Attempts}, map[string]float64{"DeliveryAccepted": 1, "DeliveryHTTPMilliseconds": float64(time.Since(start).Milliseconds())})
						return
					}
				} else {
					io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
					response.Body.Close()
					err = fmt.Errorf("worker HTTP %d", status)
				}
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	message := "request failed"
	if err != nil {
		var e *url.Error
		if errors.As(err, &e) {
			message = e.Op + ": " + e.Err.Error()
		} else {
			message = err.Error()
		}
	}
	next := time.Now().Add(retryDelay(job.Attempts))
	if err = a.Store.RecordFailure(ctx, job.ID, status, message, next); err != nil {
		a.cfg.Meter.Emit("delivery_state_error", map[string]any{"delivery_id": job.ID, "error": err.Error()}, nil)
	}
	a.cfg.Meter.Emit("delivery_retry", map[string]any{"delivery_id": job.ID, "destination_id": job.DestinationID, "http_status": status, "attempt": job.Attempts, "next_attempt_at": protocol.Time(next), "error": message}, map[string]float64{"DeliveryRetries": 1})
}
func (a *App) observe(ctx context.Context) {
	for {
		now := time.Now()
		if s, err := a.Store.Stats(ctx, now); err == nil {
			a.cfg.Meter.Emit("queue_status", map[string]any{"stats": s}, map[string]float64{"AppHeartbeat": 1, "PendingDeliveries": float64(s.Pending), "AwaitingResult": float64(s.AwaitingResult), "FinishedDeliveries": float64(s.Finished), "OldestPendingSeconds": s.OldestPendingSeconds, "QueuedEvents": float64(s.QueuedEvents)})
		}
		raw, backupErr := a.Store.Metadata(ctx, "last_backup_success")
		if errors.Is(backupErr, store.ErrNotFound) {
			raw, backupErr = a.Store.Metadata(ctx, "backup_monitoring_started_at")
		}
		if backupErr == nil {
			if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				a.cfg.Meter.Emit("backup_age", nil, map[string]float64{"BackupAgeSeconds": max(0, now.Sub(t).Seconds())})
			}
		}
		if err := a.Store.Cleanup(ctx, now.AddDate(-1, 0, 0)); err != nil {
			a.cfg.Meter.Emit("cleanup_error", map[string]any{"error": err.Error()}, nil)
		}
		if !sleep(ctx, time.Minute) {
			return
		}
	}
}
