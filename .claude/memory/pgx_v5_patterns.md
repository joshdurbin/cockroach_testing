---
name: pgx-v5-patterns
description: pgx v5 patterns specific to CockroachDB usage in this project
metadata: 
  node_type: memory
  type: project
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

## db.New(tx) works directly — DBTX interface

sqlc generates a `DBTX` interface that both `*pgxpool.Pool` and `pgx.Tx` implement. This means you can create a `*db.Queries` bound to a transaction:

```go
tx, _ := pool.Begin(ctx)
q := db.New(tx)           // ← works, no type assertion needed
q.InsertEvent(ctx, params)
q.IncrementCounter(ctx, target)
tx.Commit(ctx)
```

No need to pass the pool separately or use `q.WithTx(tx)` — `db.New(tx)` is cleaner.

## stdlib bridge for goose

goose requires `*database/sql.DB`. pgx v5 provides a bridge:

```go
import "github.com/jackc/pgx/v5/stdlib"

pool, _ := pgxpool.NewWithConfig(ctx, cfg)
sqlDB := stdlib.OpenDBFromPool(pool)
// Pass sqlDB to goose.Up, goose.Reset, etc.
```

Close order matters: close `sqlDB` before closing the pool (`pool.Close()`).

## PrepareConn replaces BeforeAcquire in pgx v5

`BeforeAcquire` is deprecated. Use `PrepareConn` for dynamic per-connection setup:

```go
cfg.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
    _, err := conn.Exec(ctx, "SET app.current_tenant = $1", tenantID)
    return err == nil, err  // (true=keep conn, false=destroy conn)
}
```

For static setup (same for all connections), use `AfterConnect` instead.

## pgtype.UUID vs uuid.UUID in sqlc params

sqlc in pgx/v5 mode generates nullable UUID fields as `pgtype.UUID`, non-nullable as `uuid.UUID`. Building params:

```go
// Non-nullable: just use uuid.UUID directly
TenantID: tenantUUID   // uuid.UUID

// Nullable (FK that can be NULL):
TenantID: pgtype.UUID{Bytes: tenantUUID, Valid: true}   // present
TenantID: pgtype.UUID{}                                  // NULL
```

## Multi-host DSN for load balancing

pgx v5 supports multiple hosts in the connection string URL:

```
postgresql://root@host1:26257,host2:26258,host3:26259/defaultdb?sslmode=disable&load_balance_hosts=random
```

`load_balance_hosts=random` distributes connections across hosts. Each host needs its explicit port in the URL form.

## Ping retry for post-restart connections

CockroachDB accepts TCP connections immediately after container start but closes them with "unexpected EOF" while the SQL server is still initializing. Always retry the initial ping:

```go
func pingWithRetry(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) error {
    deadline := time.Now().Add(timeout)
    backoff := time.Second
    for time.Now().Before(deadline) {
        if err := pool.Ping(ctx); err == nil {
            return nil
        }
        time.Sleep(backoff)
        if backoff < 8*time.Second { backoff *= 2 }
    }
    return fmt.Errorf("pool not ready after %s", timeout)
}
```

## ConnectTimeout vs pool-level timeout

`cfg.ConnConfig.ConnectTimeout` sets per-individual-connection dial timeout. This is different from the pool-level `MaxConnLifetime`. Set it to 5s to prevent single slow nodes from blocking pool acquisition:

```go
cfg.ConnConfig.ConnectTimeout = 5 * time.Second
cfg.MaxConns = 50
cfg.MinConns = 2
cfg.MaxConnLifetime = 30 * time.Minute
cfg.MaxConnIdleTime = 5 * time.Minute
cfg.HealthCheckPeriod = 30 * time.Second
```

## SET LOCAL vs SET for session variables

`SET LOCAL var = $1` scopes the variable to the current transaction and resets automatically on commit/rollback. `SET var = $1` persists for the entire connection session.

**Always use SET LOCAL** for tenant context and QoS tier in pooled connections. If you use `SET` without LOCAL and the transaction rolls back, the variable remains set for the next query on that connection.

## Serialization failure detection

CockroachDB returns SQLSTATE 40001 for serialization failures. Check with:

```go
import "github.com/jackc/pgx/v5/pgconn"

func IsSerializationFailure(err error) bool {
    var pgErr *pgconn.PgError
    return errors.As(err, &pgErr) && pgErr.Code == "40001"
}
```

Retry the entire transaction (including SET LOCAL statements) on 40001. The retry must re-execute everything from `pool.Begin(ctx)`.

## sqlc type for Numeric/Decimal

sqlc with pgx/v5 maps `NUMERIC(10,2)` to `pgtype.Numeric` by default, which is complex to work with. Override in `sqlc.yaml` to use `string`:

```yaml
overrides:
  - db_type: "pg_catalog.numeric"
    go_type: "string"
```

Store decimal values as formatted strings (e.g. `fmt.Sprintf("%.2f", value)`), parse back with `fmt.Sscanf` or `strconv.ParseFloat`.
