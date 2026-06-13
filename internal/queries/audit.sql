-- name: InsertEvent :exec
INSERT INTO events (tenant_id, region, event_type, actor, severity, payload)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: InsertRegionalEvent :exec
INSERT INTO events_regional (tenant_id, region, event_type, actor, severity, payload)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: CountRecentEvents :one
SELECT COUNT(*) AS total
FROM events
WHERE created_at >= $1;

-- name: CountRecentRegionalEvents :one
SELECT COUNT(*) AS total
FROM events_regional
WHERE region = $1
  AND created_at >= $2;

-- name: EventRateByRegion :many
SELECT
    region,
    COUNT(*) AS count
FROM events
WHERE created_at >= $1
GROUP BY region
ORDER BY count DESC;

-- name: RegionalEventRateByType :many
SELECT
    event_type,
    COUNT(*) AS count
FROM events_regional
WHERE region = $1
  AND created_at >= $2
GROUP BY event_type
ORDER BY count DESC;
