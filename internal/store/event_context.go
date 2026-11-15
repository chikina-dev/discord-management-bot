package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
)

type EventContextSnapshot struct {
	Data     map[string]any
	Complete bool
}

func (s *Store) EventContext(ctx context.Context, eventID string) (EventContextSnapshot, error) {
	return scanEventContext(s.db.QueryRowContext(ctx, "SELECT data,complete FROM event_contexts WHERE event_id=?", eventID))
}

func scanEventContext(row scanner) (EventContextSnapshot, error) {
	var snapshot EventContextSnapshot
	var raw []byte
	if err := row.Scan(&raw, &snapshot.Complete); err != nil {
		return snapshot, err
	}
	if err := json.Unmarshal(raw, &snapshot.Data); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// CaptureEventContext advances channel observations once and freezes the event's
// decision input. Completing a missing-channel fallback never replays projection.
func (s *Store) CaptureEventContext(ctx context.Context, eventID, guildID string, channels []map[string]any, proposed EventContextSnapshot) (EventContextSnapshot, error) {
	raw, err := protocol.JSON(proposed.Data)
	if err != nil {
		return EventContextSnapshot{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventContextSnapshot{}, err
	}
	defer tx.Rollback()
	existing, err := scanEventContext(tx.QueryRowContext(ctx, "SELECT data,complete FROM event_contexts WHERE event_id=?", eventID))
	if err == nil {
		if existing.Complete || !proposed.Complete {
			return existing, nil
		}
		if _, err = tx.ExecContext(ctx, "UPDATE event_contexts SET data=?,complete=1 WHERE event_id=?", raw, eventID); err != nil {
			return EventContextSnapshot{}, err
		}
	} else {
		if !errors.Is(err, ErrNotFound) {
			return EventContextSnapshot{}, err
		}
		var position int64
		if err = tx.QueryRowContext(ctx, "SELECT position FROM event_positions WHERE event_id=?", eventID).Scan(&position); err != nil {
			return EventContextSnapshot{}, err
		}
		if err = projectChannels(ctx, tx, guildID, channels, position); err != nil {
			return EventContextSnapshot{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO event_contexts(event_id,data,complete) VALUES(?,?,?)", eventID, raw, proposed.Complete); err != nil {
			return EventContextSnapshot{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return EventContextSnapshot{}, err
	}
	return proposed, nil
}

func projectChannels(ctx context.Context, tx *sql.Tx, guildID string, channels []map[string]any, position int64) error {
	for _, channel := range channels {
		id, _ := channel["id"].(string)
		if id == "" {
			continue
		}
		var previous int64
		err := tx.QueryRowContext(ctx, "SELECT position FROM channel_positions WHERE channel_id=?", id).Scan(&previous)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil && previous > position {
			continue
		}
		merged := map[string]any{}
		var old []byte
		err = tx.QueryRowContext(ctx, "SELECT data FROM channels WHERE id=?", id).Scan(&old)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if len(old) > 0 {
			if err = json.Unmarshal(old, &merged); err != nil {
				return err
			}
		}
		for key, value := range channel {
			if key != "newly_created" {
				merged[key] = value
			}
		}
		merged["guild_id"] = guildID
		raw, err := protocol.JSON(merged)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO channels(id,guild_id,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET guild_id=excluded.guild_id,data=excluded.data", id, guildID, raw); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO channel_positions(channel_id,position) VALUES(?,?) ON CONFLICT(channel_id) DO UPDATE SET position=excluded.position", id, position); err != nil {
			return err
		}
	}
	return nil
}
