-- Schema file for sqlc code generation only.
-- Uses standard PostgreSQL DDL compatible with the sqlc parser.
-- The actual migration (with CockroachDB-specific zone configs,
-- PARTITION BY LIST, RLS, and user/grant statements) lives in
-- internal/migrations/.

CREATE TABLE IF NOT EXISTS tenants (
    id          UUID        PRIMARY KEY,
    qos_tier    TEXT        NOT NULL,
    home_region TEXT        NOT NULL DEFAULT 'us-east',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS event_counters (
    target TEXT NOT NULL PRIMARY KEY,
    cnt    BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS events (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID        REFERENCES tenants(id),
    region      TEXT        NOT NULL,
    event_type  TEXT        NOT NULL,
    actor       TEXT        NOT NULL,
    severity    TEXT        NOT NULL DEFAULT 'info',
    payload     JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- events_regional has the same logical columns; the PARTITION BY LIST
-- and zone configs are CockroachDB-specific and applied in the migration.
CREATE TABLE IF NOT EXISTS events_regional (
    region      TEXT        NOT NULL,
    id          UUID        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   UUID        REFERENCES tenants(id),
    event_type  TEXT        NOT NULL,
    actor       TEXT        NOT NULL,
    severity    TEXT        NOT NULL DEFAULT 'info',
    payload     JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (region, id)
);
