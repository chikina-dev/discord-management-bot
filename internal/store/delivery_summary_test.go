package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
)

func TestDeliverySummaryBeforeAndAfterFinalResult(t *testing.T) {
	s, destination, trigger := setup(t)
	ctx := context.Background()
	id := enqueue(t, s, destination, trigger)
	now := time.Now()
	assertSummary := func(state string, want *protocol.ResultStatus) {
		t.Helper()
		items, err := s.Deliveries(ctx, destination.ID, state, "", 100)
		if err != nil || len(items) != 1 {
			t.Fatalf("summary: %v %v", items, err)
		}
		item := items[0]
		if item.ID != id || item.State != state || len(item.Payload) != 0 || len(item.Result) != 0 {
			t.Fatalf("unexpected summary: %#v", item)
		}
		if want == nil && item.ResultStatus != nil || want != nil && (item.ResultStatus == nil || *item.ResultStatus != *want) {
			t.Fatalf("result status: got %v, want %v", item.ResultStatus, want)
		}
		body, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err = json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		status, present := wire["result_status"]
		if !present || want == nil && status != nil || want != nil && status != string(*want) {
			t.Fatalf("missing or incorrect wire status: %s", body)
		}
	}
	assertSummary("pending", nil)
	if _, err := s.PrepareDelivery(ctx, destination.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAcceptance(ctx, id, protocol.Acceptance{DeliveryID: id, Timestamps: protocol.Timestamps{WorkerAcceptedAt: protocol.Time(now)}}); err != nil {
		t.Fatal(err)
	}
	assertSummary("accepted", nil)
	result := final(destination, id, now)
	result.Status = protocol.PartialSuccess
	result.Details = json.RawMessage(`{"status":"worker-specific-value"}`)
	if _, err := s.RecordResult(ctx, result, destination.KeyID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	assertSummary("finished", &result.Status)
	detail, err := s.Delivery(ctx, id)
	if err != nil || len(detail.Payload) == 0 || len(detail.Result) == 0 || detail.ResultStatus == nil || *detail.ResultStatus != result.Status {
		t.Fatalf("detail lost data: %#v %v", detail, err)
	}
	id = enqueue(t, s, destination, trigger)
	if err := s.StopDelivery(ctx, id); err != nil {
		t.Fatal(err)
	}
	assertSummary("stopped", nil)
}
