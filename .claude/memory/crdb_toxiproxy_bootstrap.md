---
name: crdb-toxiproxy-bootstrap
description: "Ordering constraints, port wiring, and gotchas when bootstrapping CockroachDB through Toxiproxy"
metadata: 
  node_type: memory
  type: project
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

## The mandatory startup order

Toxiproxy proxies MUST be registered before CockroachDB nodes start. Nodes immediately try to contact each other via their `--advertise-addr` (which points through Toxiproxy). If Toxiproxy is running but has no proxy rules, nodes get connection refused and fail to gossip.

**Correct sequence:**
1. Start Toxiproxy container
2. Wait for Toxiproxy HTTP API to respond (`GET /proxies` returns 200)
3. Register proxies via `POST /proxies` (one per CRDB node)
4. Start CRDB nodes
5. Wait for CRDB HTTP health (`/health` returns any response)
6. Run `cockroach init`

**Why:** `EnsureToxiproxy` + `registerToxiproxyProxies` in docker/chaos.go, called from `CreateCluster` before `startCRDBNode`.

## Port layout

```
crdbRPCPort  = 26357  (each node's --listen-addr, RPC-only when --sql-addr set)
crdbSQLPort  = 26257  (each node's --sql-addr, SQL-only)
crdbHTTPPort = 8080   (each node's --http-addr, Admin UI + /health)

Toxiproxy proxy N: listen=0.0.0.0:2600N, upstream=cdbct-crdb-N:26357
  Node 1 → :26001 → crdb-1:26357
  Node 9 → :26009 → crdb-9:26357

Host port mappings (N-based offsets):
  SQL:  26256+N  (node 1 = 26257, node 9 = 26265)
  HTTP: 8079+N   (node 1 = 8080,  node 9 = 8088)
  RPC:  26356+N  (node 1 = 26357, node 9 = 26365)
```

All nodes listen on `0.0.0.0:26357` internally — no conflict because each container has its own network namespace.

## cockroach init targets the RPC port, not SQL

`cockroach init` speaks gRPC and must connect to `--listen-addr` (26357), NOT `--sql-addr` (26257). Sending init to the SQL port gives "error reading server preface: use of closed network connection". This bit us hard.

**In code:** `m.Exec(ctx, containerID, []string{"cockroach", "init", "--insecure", "--host=localhost:26357"})`

## Health check path

`/_admin/v1/health` returns 404. The correct liveness endpoint is `/health`.

Pre-init: `/health` returns 200 almost immediately (HTTP server up).  
Pre-init: SQL port accepts TCP but closes connections immediately — not safe for `cockroach init`.  
**Dual check:** poll `/health` on HTTP port AND attempt TCP dial on RPC port before calling init.

## CONFIGURE ZONE must not be in goose migrations

`ALTER PARTITION ... CONFIGURE ZONE` fails on freshly-initialized clusters because the zone subsystem isn't ready yet. Putting it in a goose migration causes the migration to fail, leaving the table created but the migration unrecorded, breaking the goose version sequence.

**Fix:** `cockroach/zone_configs.go` — apply zone configs after `goose.Up` completes, with exponential backoff retry (60s timeout). Failure is non-fatal (logs warning, workload continues without pinned replicas).

## Geo-partition resilience requires 3 nodes per region

- `num_replicas=1, constraints=[+region=X]` → loses availability when the single node in X is partitioned
- `num_replicas=3, constraints=[+region=X]` → survives 1-of-3 node loss in X (2/3 = quorum)
- Minimum resilient geo-partition: 3 nodes per region × 3 regions = 9 nodes total

**Default cluster size: 9 nodes** (`nodeRegion` cycles us-east/us-west/eu-central, so nodes 1,4,7 = us-east etc.)

## chaos status/clear must auto-discover from Toxiproxy

Don't use a `--nodes N` flag for status/clear. The actual proxy list comes from `GET /proxies` on the Toxiproxy API. Filter by `crdb-node-` prefix. This correctly handles any cluster size without requiring the user to pass a count.

**Why this matters:** with default 9 nodes, a hardcoded `--nodes 3` default silently misses nodes 4–9.

## Toxiproxy proxy registration is idempotent

`POST /proxies` returns 201 on create, 409 on conflict (already exists). Both are acceptable. This means `EnsureToxiproxy` can be called safely on restarts.
