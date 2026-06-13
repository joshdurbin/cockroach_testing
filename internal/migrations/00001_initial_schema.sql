-- +goose NO TRANSACTION
-- +goose Up

-- ============================================================
-- All tables are created with their full final schema up front.
-- No ALTER TABLE is needed on fresh starts — avoids the several-
-- second schema-change overhead per column addition.
-- ============================================================

CREATE TABLE IF NOT EXISTS tenants (
    id          UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    qos_tier    STRING  NOT NULL CHECK (qos_tier IN ('critical', 'regular', 'background')),
    home_region STRING  NOT NULL DEFAULT 'us-east',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS events (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID        REFERENCES tenants(id),
    region      STRING      NOT NULL,
    event_type  STRING      NOT NULL,
    actor       STRING      NOT NULL,
    severity    STRING      NOT NULL DEFAULT 'info',
    payload     JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_events_time     ON events (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_severity ON events (severity, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_region   ON events (region, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_tenant   ON events (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS events_regional (
    region      STRING      NOT NULL,
    id          UUID        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   UUID        REFERENCES tenants(id),
    event_type  STRING      NOT NULL,
    actor       STRING      NOT NULL,
    severity    STRING      NOT NULL DEFAULT 'info',
    payload     JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (region, id)
) PARTITION BY LIST (region) (
    PARTITION east VALUES IN ('us-east'),
    PARTITION west VALUES IN ('us-west'),
    PARTITION eu   VALUES IN ('eu-central')
);

CREATE INDEX IF NOT EXISTS idx_events_regional_time   ON events_regional (region, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_regional_tenant ON events_regional (tenant_id, created_at DESC);

-- NOTE: CONFIGURE ZONE statements are applied programmatically after
-- migration via cockroach/zone_configs.go (with retry) because the zone
-- subsystem is not ready on a freshly-initialized cluster.

-- +goose Down

DROP TABLE IF EXISTS events_regional;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS tenants;
