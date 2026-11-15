package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/chikina-dev/discord-management-bot/internal/rules"
	"github.com/chikina-dev/discord-management-bot/internal/store"
)

var errDestinationMissing = errors.New("referenced destination is missing")

func (a *App) loadCatalog(ctx context.Context) error {
	triggers, err := a.Store.Triggers(ctx)
	if err != nil {
		return err
	}
	destinations, err := a.Store.Destinations(ctx)
	if err != nil {
		return err
	}
	targets := make(map[string]store.Destination, len(destinations))
	for _, destination := range destinations {
		targets[destination.ID] = destination
	}
	next := make([]binding, 0, len(triggers))
	for _, trigger := range triggers {
		program, err := rules.Compile(trigger.Condition)
		if err != nil {
			return fmt.Errorf("trigger %s: %w", trigger.ID, err)
		}
		destination, ok := targets[trigger.DestinationID]
		if !ok {
			return fmt.Errorf("trigger %s references a missing destination", trigger.ID)
		}
		next = append(next, binding{trigger, destination, program})
	}
	a.publishCatalog(next)
	return nil
}

// Publication performs no I/O and cannot fail after the durable mutation commits.
func (a *App) publishCatalog(next []binding) {
	usesContext := false
	for _, entry := range next {
		usesContext = usesContext || entry.program.UsesContext
	}
	a.bindings = next
	a.usesContext = usesContext
	a.catalogGeneration++
}

func (a *App) putDestination(ctx context.Context, destination store.Destination, create bool) (store.Destination, error) {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	next := append([]binding(nil), a.bindings...)
	saved, err := a.Store.PutDestination(ctx, destination, create)
	if err != nil {
		return saved, err
	}
	for i := range next {
		if next[i].destination.ID == saved.ID {
			next[i].destination = saved
		}
	}
	a.publishCatalog(next)
	return saved, nil
}

func (a *App) deleteDestination(ctx context.Context, id string) error {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	next := make([]binding, 0, len(a.bindings))
	for _, entry := range a.bindings {
		if entry.destination.ID != id {
			next = append(next, entry)
		}
	}
	if err := a.Store.DeleteDestination(ctx, id); err != nil {
		return err
	}
	a.publishCatalog(next)
	return nil
}

func (a *App) putTrigger(ctx context.Context, trigger store.Trigger, create bool) (store.Trigger, error) {
	program, err := rules.Compile(trigger.Condition)
	if err != nil {
		return trigger, err
	}
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	if !create {
		if _, err := a.Store.Trigger(ctx, trigger.ID); err != nil {
			return trigger, err
		}
	}
	destination, err := a.Store.Destination(ctx, trigger.DestinationID)
	if errors.Is(err, store.ErrNotFound) {
		return trigger, errDestinationMissing
	}
	if err != nil {
		return trigger, err
	}
	next := make([]binding, 0, len(a.bindings)+1)
	for _, entry := range a.bindings {
		if entry.trigger.ID != trigger.ID {
			next = append(next, entry)
		}
	}
	next = append(next, binding{trigger, destination, program})
	saved, err := a.Store.PutTrigger(ctx, trigger, create)
	if err != nil {
		return saved, err
	}
	next[len(next)-1].trigger = saved
	a.publishCatalog(next)
	return saved, nil
}

func (a *App) deleteTrigger(ctx context.Context, id string) error {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	next := make([]binding, 0, len(a.bindings))
	for _, entry := range a.bindings {
		if entry.trigger.ID != id {
			next = append(next, entry)
		}
	}
	if err := a.Store.DeleteTrigger(ctx, id); err != nil {
		return err
	}
	a.publishCatalog(next)
	return nil
}
