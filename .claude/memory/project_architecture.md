---
name: project-architecture
description: Key architectural decisions in cdbct and the reasoning behind them
metadata: 
  node_type: memory
  type: project
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

## Binary serves dual roles

`cdbct` is both the host CLI and the workload container entrypoint. The `--dsn` flag on `workload start` switches modes:
- No `--dsn`: host mode → build Docker image, start container
- `--dsn=<url>`: container mode → run workload in-process

This avoids a separate binary while keeping the container image minimal (single static binary + alpine).

## Two-pool pattern for workload

The workload maintains two pgx pools:
- `adminPool` (root): runs migrations, seeds tenants, used by gRPC for cross-tenant admin queries
- `appPool` (appuser): all event inserts, subject to RLS, per-transaction QoS

Derive appPool DSN from adminPool DSN by replacing the username.

## Workload connect pattern

`cockroach.Connect()` runs goose migrations and returns a pool. `cockroach.ConnectRaw()` opens a pool without migrations — used for the appuser pool and for gRPC verification queries.

## Container networking: SQL bypasses chaos

- CRDB `--advertise-addr` points through Toxiproxy → all inter-node Raft RPC is injectable
- CRDB `--advertise-sql-addr` points directly to the container → SQL connections bypass Toxiproxy
- This is intentional: chaos targets replication, not client traffic

## Prometheus scrape config is templated at obs setup time

`renderPromConfig(nodes []NodeInfo)` generates the Prometheus config from the live node list at `obs setup` / `quickstart` time. It's injected via `CopyToContainer` (tar stream) before the Prometheus container starts — no volume mounts, no busybox sidecars, no shell commands.

## Grafana config is embedded in the binary

`internal/docker/grafana/` contains dashboard JSON and provisioning YAML embedded via `//go:embed`. Injected via `CopyToContainer` before Grafana starts. Anonymous admin access (`GF_AUTH_ANONYMOUS_ENABLED=true`).

## gRPC server lives in the workload container

TenantService is registered with reflection — `grpcurl -plaintext localhost:9092 list` works with no .proto file. Server starts after both pools and the tenant pool are ready. Prometheus metrics on :9091 (HTTP), gRPC on :9092 (separate listener).

## event_counters: write-side accounting

The ListTenants gRPC handler reads from `event_counters` (4-row PK scan). No background goroutine, no app cache, no table scans. Counters are maintained inside each insert transaction via `IncrementCounter`. Do NOT use `crdb_internal.tables.estimated_row_count` — it raises SQLSTATE 42501 in CockroachDB v26 (access restricted even for root in some contexts).

## Five gRPC methods on TenantService

1. `ListTenants` — full pool, QoS + region distribution, event counts from counters
2. `VerifyRLS` — samples one tenant per HOME REGION (not QoS tier), proves RLS isolation
3. `GetTenantEvents` — RLS-filtered count for a specific tenant UUID
4. `GetRegionStatus` — per-region tenant count, tier distribution, event count; most useful during chaos
5. `ListTenantsByRegion` — tenants homed to a specific region for SLA communication

## Tenant home_region routing

Each tenant has `HomeRegion`. Workload writes TWO things per tick:
1. `events` (global) — with `region = tenant.HomeRegion` as label
2. `events_regional` — ONLY to tenant's home partition (not all three)

This makes the chaos demo meaningful: east-homed tenants' regional writes fail when east loses quorum. West/eu-homed tenants are unaffected.

## Zone configs applied after migrations with retry

`cockroach/zone_configs.go::ApplyZoneConfigs()` is called by the workload runner after `goose.Up`. Each `CONFIGURE ZONE` retries with exponential backoff up to 60s. Failure is non-fatal — the workload runs, just without replica pinning.

## chaos status/clear are cluster-size-agnostic

Status and ClearAll call `GET /proxies` on Toxiproxy and filter by the `crdb-node-` prefix. The `--nodes` flag only applies to `chaos setup` (proxy registration). This is correct for variable cluster sizes.

## Destroy purges by default

`cdbct destroy` always deletes data volumes. `--retain-data-volumes` preserves them. This ensures every `quickstart` begins from a clean schema state.
