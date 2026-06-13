# Adding a CockroachDB Migration

Use this when adding schema changes to `internal/migrations/`.

---

## Checklist

### 1. Always use `-- +goose NO TRANSACTION`

```sql
-- +goose NO TRANSACTION
-- +goose Up
CREATE TABLE ...;
CREATE INDEX ...;
```

Without it, goose wraps all statements in one transaction. CockroachDB rejects
`CREATE INDEX` on a table created in the same transaction ("table is being added").
`StatementBegin/StatementEnd` sends the whole block as one query — CockroachDB
closes the connection with "unexpected EOF" on large payloads.

### 2. Put ALL columns in the original CREATE TABLE

`ALTER TABLE ADD COLUMN` is a CockroachDB schema change job — 3-6 seconds per
column on an empty table, longer with regional latency injected. On a fresh start,
there's no reason to use ALTER TABLE. Include every column needed upfront.

```sql
-- WRONG: split across migrations
-- 00001: CREATE TABLE events (id, region, ...)
-- 00002: ALTER TABLE events ADD COLUMN tenant_id ...  ← slow, unnecessary

-- CORRECT: all columns from the start
-- 00001: CREATE TABLE events (id, tenant_id, region, ...)
```

### 3. CONFIGURE ZONE goes in Go code, not migrations

`ALTER PARTITION ... CONFIGURE ZONE` fails on freshly-initialized clusters
because the zone subsystem isn't ready. Put it in `cockroach/zone_configs.go`
and call `ApplyZoneConfigs` after `goose.Up` with retry logic.

### 4. Never modify an applied migration

If a migration has been applied to any volume, changing it causes goose version
drift: the recorded version is old schema, the next migration fails expecting
columns that don't exist. Create a new migration instead.

Exception: `-- +goose NO TRANSACTION` + `SELECT 1` no-op migrations are safe
to keep as version placeholders when superseding an earlier migration.

### 5. Update sqlc schema.sql separately

sqlc uses a PostgreSQL parser — it doesn't understand:
- `PARTITION BY LIST`
- `CONFIGURE ZONE`
- `STRING` type (use `TEXT`)
- Inline `INDEX` clauses in `CREATE TABLE`

Maintain `internal/queries/schema.sql` with standard PostgreSQL DDL that
mirrors the final table shape. After editing queries, run `make generate`.

### 6. Run `make docker-clean && cdbct quickstart` to verify

The migration runs against a fresh cluster. Watch for:
- Total migration time > 15s → probably ALTER TABLE or missing NO TRANSACTION
- "relation X does not exist" in 00002+ → columns missing from 00001 CREATE TABLE
- goose version drift error → volumes not cleaned before test

---

## Template

```sql
-- +goose NO TRANSACTION
-- +goose Up

CREATE TABLE IF NOT EXISTS my_table (
    id          UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    -- all columns here, never ALTER TABLE later
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_my_table_created ON my_table (created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS my_table;
```

---

## Key files

- `internal/migrations/` — all migration files
- `internal/queries/schema.sql` — sqlc schema (PostgreSQL DDL only)
- `internal/cockroach/zone_configs.go` — CONFIGURE ZONE with retry
- `internal/cockroach/client.go` — goose.Up call
