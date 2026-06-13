-- name: InsertTenant :exec
INSERT INTO tenants (id, qos_tier, home_region)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO NOTHING;

-- name: LoadTenants :many
SELECT id, qos_tier, home_region, created_at
FROM tenants
ORDER BY home_region, qos_tier, id;

-- name: LoadTenantsByRegion :many
SELECT id, qos_tier, home_region, created_at
FROM tenants
WHERE home_region = $1
ORDER BY qos_tier, id;

-- name: CountTenantsByRegion :many
SELECT home_region, COUNT(*) AS total
FROM tenants
GROUP BY home_region
ORDER BY home_region;

-- name: CountTenants :one
SELECT COUNT(*) AS total FROM tenants;

-- name: GetCounters :many
-- Returns per-target write counts from the event_counters table.
-- These are maintained via UPDATE inside each insert transaction —
-- a 4-row primary-key scan, no full table scan required.
SELECT target, cnt FROM event_counters ORDER BY target;

-- name: IncrementCounter :exec
-- Increments the counter for one target within the calling transaction.
UPDATE event_counters SET cnt = cnt + 1 WHERE target = $1;

-- name: CountAllEvents :one
SELECT COUNT(*) AS total FROM events;

-- name: CountAllRegionalEvents :one
SELECT COUNT(*) AS total FROM events_regional;

-- name: CountTenantEvents :one
SELECT COUNT(*) AS total
FROM events
WHERE tenant_id = $1;

-- name: EventsPerTenant :many
SELECT tenant_id, COUNT(*) AS total
FROM events
WHERE tenant_id IS NOT NULL
GROUP BY tenant_id
ORDER BY total DESC;
