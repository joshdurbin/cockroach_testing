-- +goose NO TRANSACTION
-- +goose Up

-- ============================================================
-- Composite indexes for the RLS-filtered time-range read pattern.
--
-- runReads issues: SELECT COUNT(*) FROM <table> WHERE created_at >= $1
-- With RLS active, CockroachDB rewrites this as:
--   WHERE tenant_id = <uuid> AND created_at >= $1
--
-- A composite (tenant_id, created_at) index turns that into an
-- index-only range scan rather than a full table scan.  At low tenant
-- counts the impact is minor; at 1000+ tenants it avoids scanning every
-- row in the table to count one tenant's recent events.
-- ============================================================
CREATE INDEX IF NOT EXISTS idx_events_tenant_time
    ON events (tenant_id, created_at);

-- events_regional: same pattern plus a region equality filter.
-- Partition pruning already limits the scan to one region, but the
-- composite index avoids scanning all tenant rows within that partition.
CREATE INDEX IF NOT EXISTS idx_events_regional_tenant_time
    ON events_regional (tenant_id, created_at);

-- +goose Down
DROP INDEX IF EXISTS events@idx_events_tenant_time;
DROP INDEX IF EXISTS events_regional@idx_events_regional_tenant_time;
