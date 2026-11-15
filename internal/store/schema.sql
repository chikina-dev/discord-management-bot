CREATE TABLE IF NOT EXISTS destinations (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, url TEXT NOT NULL,
  result_public_key TEXT NOT NULL, result_key_id TEXT NOT NULL,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS destinations_key ON destinations(result_key_id);
CREATE TABLE IF NOT EXISTS triggers (
  id TEXT PRIMARY KEY,
  destination_id TEXT NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
  name TEXT NOT NULL, condition TEXT NOT NULL,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
  UNIQUE(destination_id,name)
);
CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS channels (
  id TEXT PRIMARY KEY, guild_id TEXT NOT NULL, data BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, session_id TEXT NOT NULL, sequence INTEGER NOT NULL,
  event_type TEXT NOT NULL, data BLOB NOT NULL, received_at TEXT NOT NULL,
  supplied_context BLOB, trigger_ids BLOB, next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  UNIQUE(session_id,sequence)
);
CREATE INDEX IF NOT EXISTS events_ready ON events(next_attempt_at);
CREATE TABLE IF NOT EXISTS event_positions (
  position INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id TEXT NOT NULL UNIQUE REFERENCES events(id) ON DELETE CASCADE
);
CREATE TRIGGER IF NOT EXISTS assign_event_position AFTER INSERT ON events BEGIN
  INSERT INTO event_positions(event_id) VALUES(new.id);
END;
INSERT INTO event_positions(event_id)
  SELECT id FROM events WHERE id NOT IN (SELECT event_id FROM event_positions) ORDER BY rowid;
CREATE TABLE IF NOT EXISTS event_contexts (
  event_id TEXT PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
  data BLOB NOT NULL, complete INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_positions (
  channel_id TEXT PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
  position INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS deliveries (
  id TEXT PRIMARY KEY, event_id TEXT NOT NULL, trigger_id TEXT NOT NULL,
  trigger_name TEXT NOT NULL, condition_snapshot TEXT NOT NULL,
  destination_id TEXT NOT NULL, destination_name TEXT NOT NULL,
  url_snapshot TEXT NOT NULL, result_public_key TEXT NOT NULL, result_key_id TEXT NOT NULL,
  event_type TEXT NOT NULL, payload BLOB NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','accepted','stopped','finished')),
  created_at TEXT NOT NULL, first_sent_at TEXT NOT NULL DEFAULT '',
  worker_accepted_at TEXT NOT NULL DEFAULT '', worker_finished_at TEXT NOT NULL DEFAULT '',
  result_received_at TEXT NOT NULL DEFAULT '', stopped_at TEXT NOT NULL DEFAULT '',
  stop_reason TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0, lease_until INTEGER NOT NULL DEFAULT 0,
  last_http_status INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
  result BLOB, result_hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS deliveries_ready ON deliveries(state,next_attempt_at,lease_until);
CREATE INDEX IF NOT EXISTS deliveries_destination ON deliveries(destination_id,state);
CREATE INDEX IF NOT EXISTS deliveries_trigger ON deliveries(trigger_id,state);
CREATE INDEX IF NOT EXISTS deliveries_history ON deliveries(created_at,id);
CREATE INDEX IF NOT EXISTS deliveries_key ON deliveries(result_key_id);
