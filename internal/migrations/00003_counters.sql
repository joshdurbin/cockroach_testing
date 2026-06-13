-- +goose NO TRANSACTION
-- +goose Up

-- ============================================================
-- event_counters: per-target write counts maintained in-place.
--
-- Each workload insert runs UPDATE event_counters SET cnt = cnt + 1
-- inside the same transaction. This is a primary-key point update —
-- zero extra round-trips, no table scan, always current.
--
-- Replaces SELECT COUNT(*) FROM events / events_regional (full table
-- scans) for the ListTenants summary. Four rows total.
-- ============================================================
CREATE TABLE IF NOT EXISTS event_counters (
    target  STRING  NOT NULL,
    cnt     INT     NOT NULL DEFAULT 0,
    PRIMARY KEY (target)
);

INSERT INTO event_counters (target, cnt) VALUES
    ('global', 0),
    ('east',   0),
    ('west',   0),
    ('eu',     0)
ON CONFLICT (target) DO NOTHING;

-- appuser increments counters on every insert.
GRANT SELECT, UPDATE ON TABLE event_counters TO appuser;

-- +goose Down
DROP TABLE IF EXISTS event_counters;
