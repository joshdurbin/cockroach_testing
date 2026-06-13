---
name: workload-container-design
description: "Dual-mode binary, ticker rate control, tenantInsert pattern, and workload container architecture"
metadata: 
  node_type: memory
  type: project
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

## Dual-mode binary: CLI host vs container entrypoint

The same `cdbct` binary serves two roles. The `--dsn` flag on `workload start` is the switch:

```
No --dsn  → host mode: build Docker image, start container via Docker SDK
--dsn=URL → container mode: run workload in-process (this is what the container exec's)
```

The container CMD is built in `docker/workload.go`:
```go
cmd := []string{
    "workload", "start",
    "--dsn", InternalDSN(opts.Nodes),  // internal Docker hostnames
    "--interval", opts.Interval.String(),
    "--tenants", strconv.Itoa(tenantCount),
    "--grpc-addr", fmt.Sprintf(":%d", WorkloadGRPCContainerPort),
    // ...
}
```

**Why this works:** cobra/viper read `--dsn` at flag parse time. Host invocation never sets `--dsn` so it falls through to the Docker branch.

## Ticker-based rate control — never goroutine pool hammering

Bad pattern (goroutine pool at full speed):
```go
for {
    pool.Submit(func() { insertOne(ctx) })  // races as fast as the pool allows
}
```

Correct pattern (ticker controls rate):
```go
ticker := time.NewTicker(cfg.Interval)  // 100ms = 10/s
for range ticker.C {
    go runWrites(ctx, pool, tenants, cfg.BatchSize)  // non-blocking, fires at interval
}
```

**Critical:** `go runWrites(...)` must be a goroutine. If `runWrites` blocks the ticker loop (slow DB), Go's Ticker drops missed ticks rather than queuing them — causing visible gaps in Grafana. Dispatching to a goroutine lets the ticker fire independently of write latency.

## tenantInsert: atomic unit of work

Every write is a single transaction containing three operations:

```go
func tenantInsert(ctx context.Context, pool *pgxpool.Pool, t Tenant, target string, fn func(*db.Queries) error) error {
    tx, err := pool.Begin(ctx)
    // 1. Set tenant context (RLS enforcement)
    tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", t.ID.String())
    // 2. Set admission control tier
    tx.Exec(ctx, "SET LOCAL default_transaction_quality_of_service = $1", t.QoSTier)
    // 3. The actual INSERT
    q := db.New(tx)
    fn(q)
    // 4. Increment write-side counter (PK point update, zero extra cost)
    q.IncrementCounter(ctx, target)
    tx.Commit(ctx)
}
```

On serialization failure (40001), retry the whole function including SET LOCAL — those statements must be inside the retried transaction.

## Two pools: admin vs appuser

```go
// Admin pool: root, runs migrations and seeds tenants
adminPool, _ := cockroach.Connect(ctx, cfg.DSN)

// Appuser pool: derived from admin DSN, subject to RLS
appDSN := AppUserDSN(cfg.DSN)   // replace "root@" with "appuser@"
appPool, _ := cockroach.ConnectRaw(ctx, appDSN)
```

`cockroach.Connect` runs goose migrations. `cockroach.ConnectRaw` opens a pool without migrations (used for appuser and gRPC admin queries).

## AppUserDSN derivation

```go
func AppUserDSN(adminDSN string) string {
    parts := strings.SplitN(adminDSN, "://", 2)
    if len(parts) == 2 {
        authority := parts[1]
        if at := strings.Index(authority, "@"); at >= 0 {
            return parts[0] + "://appuser@" + authority[at+1:]
        }
    }
    return adminDSN
}
```

## InternalDSN vs host DSN

The workload container cannot use `localhost:26257` (that's the host machine's port mapping). It must use Docker container hostnames:

```go
func InternalDSN(nodes int) string {
    hosts := make([]string, nodes)
    for i := range nodes {
        hosts[i] = fmt.Sprintf("cdbct-crdb-%d:%d", i+1, crdbSQLPort)
    }
    return fmt.Sprintf(
        "postgresql://root@%s/defaultdb?sslmode=disable&load_balance_hosts=random",
        strings.Join(hosts, ","),
    )
}
```

## gRPC server startup timing

The gRPC server must start AFTER both pools and the TenantPool are ready. If it starts before tenant seeding completes, ListTenants returns empty results.

```go
// In runner.go Run():
adminPool, _ := cockroach.Connect(ctx, cfg.DSN)   // runs migrations
ApplyZoneConfigs(ctx, adminPool.Pool)
tenants, _ := SeedOrLoad(ctx, adminPool.Pool, cfg.TenantCount)
appPool, _ := cockroach.ConnectRaw(ctx, appDSN)
// NOW start gRPC — all dependencies are ready
go ServeGRPC(cfg.GRPCAddr, tenants, adminPool.Pool, appPool)
```

## workload.Dockerfile multi-stage pattern

```dockerfile
FROM golang:alpine AS builder
ENV GOTOOLCHAIN=auto     # downloads correct Go version if image is older than go.mod requires
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download      # layer cached separately from source
COPY . .
RUN CGO_ENABLED=0 go build -o /cdbct .

FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /cdbct /usr/local/bin/cdbct
ENTRYPOINT ["cdbct"]
```

`GOTOOLCHAIN=auto` is essential — without it, a `golang:alpine` image older than the `go` directive in `go.mod` will refuse to build.

## Home-region routing: each tenant writes to their partition only

Each tenant has a `HomeRegion` field. Workload writes are routed to that region's partition — NOT to all partitions.

```go
// WRONG — old pattern, all tenants write to all 3 partitions
for _, region := range regions {
    doWrite(regionTarget(region), ...)  // east, west, eu for every tenant
}

// CORRECT — route to tenant's home partition only
doWrite("global", func(t Tenant) error {
    return insertEvent(ctx, pool, t, "global", t.HomeRegion)  // region label = home region
})
homeTarget := regionTarget(t.HomeRegion)  // east | west | eu
doWrite(homeTarget, func(t Tenant) error {
    return insertRegionalEvent(ctx, pool, t, homeTarget, t.HomeRegion)
})
```

This means:
- East-homed tenants only write to the east partition
- When east loses quorum, only east-homed tenants' regional writes fail
- `GetRegionStatus` tells you exactly which tenants are impacted

## TenantPool indexes by both tier and region

TenantPool now has `byRegion map[string][]Tenant` alongside `byTier`. Used by:
- `GetRegionStatus` gRPC method (tenant distribution per region)
- `ListTenantsByRegion` gRPC method

## TenantPool: seed-or-load pattern

```go
func SeedOrLoad(ctx context.Context, pool *pgxpool.Pool, n int) (*TenantPool, error) {
    count, _ := q.CountTenants(ctx)
    if count == 0 {
        seed(ctx, q, n)    // generate N UUIDs, distribute 20/60/20 across tiers
    }
    return load(ctx, q)    // always load from DB into memory
}
```

The in-memory pool is a slice of `Tenant{ID uuid.UUID, QoSTier string}` used by the ticker to randomly pick a tenant per write. Avoids querying the DB per insert.
