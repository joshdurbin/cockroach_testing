-- +goose NO TRANSACTION
-- +goose Up

-- ============================================================
-- Application user, grants, and Row-Level Security.
-- tenant_id and home_region columns are already in 00001 — no
-- ALTER TABLE needed, which avoids schema-change overhead.
-- ============================================================

CREATE USER IF NOT EXISTS appuser;

GRANT SELECT           ON TABLE tenants          TO appuser;
GRANT SELECT, INSERT   ON TABLE events           TO appuser;
GRANT SELECT, INSERT   ON TABLE events_regional  TO appuser;

ALTER TABLE events          ENABLE ROW LEVEL SECURITY;
ALTER TABLE events          FORCE  ROW LEVEL SECURITY;
ALTER TABLE events_regional ENABLE ROW LEVEL SECURITY;
ALTER TABLE events_regional FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON events;
DROP POLICY IF EXISTS tenant_isolation ON events_regional;

CREATE POLICY tenant_isolation ON events
    FOR ALL TO appuser
    USING     (tenant_id = NULLIF(current_setting('app.current_tenant', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant', true), '')::UUID);

CREATE POLICY tenant_isolation ON events_regional
    FOR ALL TO appuser
    USING     (tenant_id = NULLIF(current_setting('app.current_tenant', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant', true), '')::UUID);

-- +goose Down

DROP POLICY IF EXISTS tenant_isolation ON events_regional;
DROP POLICY IF EXISTS tenant_isolation ON events;

ALTER TABLE events_regional DISABLE ROW LEVEL SECURITY;
ALTER TABLE events          DISABLE ROW LEVEL SECURITY;

DROP USER IF EXISTS appuser;
