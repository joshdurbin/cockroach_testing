---
name: crdb-schema-patterns
description: "Goose migration quirks, CockroachDB DDL constraints, RLS setup, and schema design lessons"
metadata: 
  node_type: memory
  type: project
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

## goose with CockroachDB: always use NO TRANSACTION

```sql
-- +goose NO TRANSACTION
-- +goose Up
CREATE TABLE ...;
CREATE INDEX ...;
```

**Why:** Without `NO TRANSACTION`, goose wraps all statements in one transaction. CockroachDB rejects `CREATE INDEX` on a table created in the same transaction ("table is being added"). With `NO TRANSACTION`, each statement is autocommitted independently.

**Never use StatementBegin/StatementEnd** for CockroachDB migrations — goose sends the entire block as a single query, which CockroachDB closes with "unexpected EOF" when the payload is large.

## CONFIGURE ZONE does not belong in goose migrations

See [[crdb-toxiproxy-bootstrap]] — zone configs fail on fresh clusters. Apply them in Go code with retry after `goose.Up`.

## goose version drift

If a migration is changed after being applied to a volume, goose records the old version as applied and skips to the next migration, which will fail expecting tables from the rewritten migration. Fix: `goose.Reset()` then `goose.Up()` on failure. Better fix: don't modify applied migrations — create new ones.

## sqlc with CockroachDB-specific syntax

sqlc uses a PostgreSQL parser and does NOT understand:
- `PARTITION BY LIST`
- `CONFIGURE ZONE`
- `STRING` type (use `TEXT`)
- Inline `INDEX` clauses

**Fix:** Maintain a separate `internal/queries/schema.sql` with standard PostgreSQL DDL for sqlc. Keep CockroachDB-specific DDL in goose migrations only.

## RLS setup for multi-tenant SaaS

Three components required:

```sql
-- 1. Enable on the table (root still bypasses unless FORCE is added)
ALTER TABLE events ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

-- 2. Create the application user (insecure mode: no password needed)
CREATE USER IF NOT EXISTS appuser;
GRANT SELECT, INSERT ON TABLE events TO appuser;

-- 3. Create the policy (NULLIF handles missing/empty setting gracefully)
DROP POLICY IF EXISTS tenant_isolation ON events;
CREATE POLICY tenant_isolation ON events
    FOR ALL TO appuser
    USING     (tenant_id = NULLIF(current_setting('app.current_tenant', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.current_tenant', true), '')::UUID);
```

**Session variable injection per-transaction (pgx v5):**
```go
tx, _ := pool.Begin(ctx)
tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID.String())
tx.Exec(ctx, "SET LOCAL default_transaction_quality_of_service = $1", qosTier)
// INSERT ...
tx.Commit(ctx)
```

`SET LOCAL` scopes to the transaction and resets automatically on commit/rollback — no context leaks between pooled connections. This is the production-correct pattern.

## Write-side counters replace COUNT(*) scans

For table totals, `SELECT COUNT(*)` is a full table scan. Replace with:

```sql
CREATE TABLE event_counters (target STRING PRIMARY KEY, cnt INT NOT NULL DEFAULT 0);
-- In the same transaction as the INSERT:
UPDATE event_counters SET cnt = cnt + 1 WHERE target = $1;
```

This is a primary-key UPDATE (zero scan cost), atomically consistent, and survives retries correctly because the retry replays the entire transaction including the counter increment.

## crdb_internal.tables is restricted — do not use for row counts

`crdb_internal.tables.estimated_row_count` raises SQLSTATE 42501 ("Access to crdb_internal and system is restricted") even for the root user in CockroachDB v26 in certain contexts. Do not rely on it.

**Use write-side counters instead.** The `event_counters` table maintained atomically in each insert transaction is exact, always current, and requires no privileges beyond the table itself. This is the correct pattern.

**Why:** `crdb_internal` access was tightened in v22.2+ and varies by version and privilege configuration. Avoid it in application code.

## Two connection pools: root + appuser

Migrations must run as `root` (CREATE USER, GRANT, CONFIGURE ZONE require superuser). Workload inserts run as `appuser` (subject to RLS). Derive the appuser DSN from the root DSN by replacing the username:

```go
func AppUserDSN(adminDSN string) string {
    // Replace user in postgresql://root@host/db → postgresql://appuser@host/db
}
```

## Geo-partitioning best practices

- PK must start with the partition column: `PRIMARY KEY (region, id)`
- Use `gen_random_uuid()` for the id portion — scatters writes within each partition
- `CONFIGURE ZONE` with `constraints = '[+region=X]'` requires nodes started with `--locality=region=X`
- For resilience: `num_replicas=3` with 3 nodes per region; `num_replicas=1` is a deliberate single-point-of-failure demo
- Minimum resilient geo-partition: 3 nodes per region × 3 regions = 9 nodes total (default)

## Consolidate schema into CREATE TABLE — avoid ALTER TABLE on fresh starts

`ALTER TABLE ADD COLUMN` is a CockroachDB schema change job. Even on an empty table it takes 3-6 seconds per column. With geo-distributed latency (39-57ms per Raft hop), each ALTER TABLE takes even longer.

**Put all columns in the original CREATE TABLE in migration 00001.** Don't split schema additions across later migrations for fresh-start tools. If adding a column to an existing deployment, use a new migration — but for tools that always start fresh (destroy + quickstart), consolidate everything upfront.

This reduced total migration time from ~70s to ~15s.

## Migrations run after regional latency injection = slow

Regional latency toxics (39-57ms per Toxiproxy proxy) are injected at the Raft RPC level. Schema changes require Raft consensus — every CREATE TABLE/INDEX round-trip through Toxiproxy adds latency. With 9 nodes and 57ms proxy latency, each DDL statement takes ~4-6s instead of <1s.

**Do not inject latency before migrations.** If latency must be injected at cluster startup (as in quickstart), run migrations before injecting. goose.Up is instant on subsequent runs (already applied), so the workload container is unaffected.
