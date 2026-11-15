package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
)

func TestDeliveryPinsRouteAndTrustAtFirstSend(t *testing.T) {
	s, destination, trigger := setup(t)
	ctx := context.Background()
	oldID := enqueue(t, s, destination, trigger)
	newID := enqueue(t, s, destination, trigger)
	now := time.Now()
	first, err := s.PrepareDelivery(ctx, destination.ID, now)
	if err != nil || first.ID != oldID {
		t.Fatalf("initial send: %v", err)
	}
	if err := s.RecordFailure(ctx, oldID, 0, "acceptance response lost", now); err != nil {
		t.Fatal(err)
	}
	newPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	updated := destination
	updated.URL = "https://new-worker.example/events"
	updated.ResultPublicKey = protocol.EncodePublicKey(newPublic)
	updated, err = s.PutDestination(ctx, updated, false)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.PrepareDelivery(ctx, destination.ID, now.Add(time.Second))
	if err != nil || retry.URL != first.URL || retry.KeyID != first.KeyID || string(retry.Payload) != string(first.Payload) {
		t.Fatalf("retry changed the delivery's route/trust/body: %+v, %v", retry, err)
	}
	if err := s.RecordAcceptance(ctx, oldID, protocol.Acceptance{DeliveryID: oldID, Timestamps: protocol.Timestamps{WorkerAcceptedAt: protocol.Time(now)}}); err != nil {
		t.Fatal(err)
	}
	next, err := s.PrepareDelivery(ctx, destination.ID, now.Add(2*time.Second))
	if err != nil || next.ID != newID || next.URL != updated.URL || next.KeyID != updated.KeyID {
		t.Fatalf("unsent delivery did not use new registration: %+v, %v", next, err)
	}
	if _, err := s.RecordResult(ctx, final(destination, oldID, now), updated.KeyID, now); !errors.Is(err, ErrResultOwner) {
		t.Fatalf("new key can sign old delivery's result: %v", err)
	}
	if _, err := s.RecordResult(ctx, final(updated, newID, now), destination.KeyID, now); !errors.Is(err, ErrResultOwner) {
		t.Fatalf("old key can sign new delivery's result: %v", err)
	}
	if _, err := s.ResultKey(ctx, destination.KeyID); err != nil {
		t.Fatalf("in-flight old key disappeared after registration update: %v", err)
	}
	if err := s.DeleteDestination(ctx, destination.ID); err != nil {
		t.Fatal(err)
	}
	oldHistory, _ := s.Delivery(ctx, oldID)
	newHistory, _ := s.Delivery(ctx, newID)
	if oldHistory.KeyID != destination.KeyID || newHistory.KeyID != updated.KeyID {
		t.Fatal("deletion rewrote historical trust")
	}
	if ok, err := s.RecordResult(ctx, final(destination, oldID, now), destination.KeyID, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("late old result: %v", err)
	}
}

func TestResultAuthorizationUsesCommittedFirstSendKey(t *testing.T) {
	s, destination, trigger := setup(t)
	ctx := context.Background()
	id := enqueue(t, s, destination, trigger)
	stale, err := s.Delivery(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	updated := destination
	updated.ResultPublicKey = protocol.EncodePublicKey(public)
	updated, err = s.PutDestination(ctx, updated, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := s.PrepareDelivery(ctx, destination.ID, now); err != nil {
		t.Fatal(err)
	}
	result := final(destination, id, now)
	if _, err := s.RecordResult(ctx, result, stale.KeyID, now); !errors.Is(err, ErrResultOwner) {
		t.Fatalf("stale enqueue snapshot authorized a differently pinned delivery: %v", err)
	}
	job, _ := s.Delivery(ctx, id)
	if job.State == "finished" || len(job.Result) != 0 {
		t.Fatal("unauthorized callback changed result")
	}
	if ok, err := s.RecordResult(ctx, result, updated.KeyID, now); err != nil || !ok {
		t.Fatalf("actual first-send signer rejected: %v", err)
	}
}
