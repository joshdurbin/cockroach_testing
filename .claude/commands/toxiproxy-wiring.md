# Toxiproxy ↔ CockroachDB Wiring

Use this when modifying `internal/docker/chaos.go`, `internal/docker/cluster.go`,
or anything that touches how Toxiproxy sits between CRDB nodes.

---

## The mandatory startup order

**Toxiproxy proxies MUST be registered before CockroachDB nodes start.**

Nodes immediately try to gossip via `--advertise-addr` (which points through Toxiproxy).
If the proxies don't exist yet, connections are refused and the cluster never forms.

Correct sequence in `CreateCluster`:
1. `EnsureToxiproxy` → start container
2. `waitForToxiproxyAPI` → poll `GET /proxies` until 200
3. `registerToxiproxyProxies` → `POST /proxies` for each node
4. `startCRDBNode` for each node
5. `waitForNodes` → poll `GET /health` on HTTP port + TCP dial on RPC port
6. `initCluster` → `cockroach init --host=localhost:26357`

---

## Port layout (9-node default)

```
crdbRPCPort  = 26357  all nodes listen on this INSIDE their container
crdbSQLPort  = 26257  all nodes listen on this INSIDE their container
crdbHTTPPort = 8080   all nodes listen on this INSIDE their container

Host-mapped ports (offset by node index):
  SQL:  26256+N  (node 1=26257, node 9=26265)
  HTTP: 8079+N   (node 1=8080,  node 9=8088)
  RPC:  26356+N  (node 1=26357, node 9=26365)

Toxiproxy proxy ports:
  Node N: listen=cdbct-toxiproxy:2600N, upstream=cdbct-crdb-N:26357
  (Node 1=26001, Node 9=26009)
```

No port conflicts: each container has its own network namespace, so all
nodes can listen on 0.0.0.0:26357 without collision.

---

## CRDB node startup flags

```go
"--listen-addr=0.0.0.0:26357"                         // RPC-only when --sql-addr is set
"--sql-addr=0.0.0.0:26257"                             // SQL-only
"--http-addr=0.0.0.0:8080"                             // Admin UI + /health
"--advertise-addr=cdbct-toxiproxy:2600N"               // all inter-node RPC through Toxiproxy
"--advertise-sql-addr=cdbct-crdb-N:26257"              // SQL connects directly, bypasses Toxiproxy
"--join=cdbct-toxiproxy:26001,...,cdbct-toxiproxy:2600N"
"--locality=region=us-east,az=az1"                     // cycles us-east/us-west/eu-central
"--cache=128MiB --max-sql-memory=256MiB"               // required on Colima to prevent OOM restarts
```

---

## cockroach init targets the RPC port, NOT the SQL port

`cockroach init` speaks gRPC. Sending it to the SQL port (26257) gives:
> error reading server preface: use of closed network connection

**Always use the RPC port inside the container:**
```go
m.Exec(ctx, containerID, []string{
    "cockroach", "init", "--insecure",
    fmt.Sprintf("--host=localhost:%d", CRDBInternalSQLPort), // 26357, not 26257
})
```

Wait — `CRDBInternalSQLPort` is exported as 26257 for SQL. The init command
needs `crdbRPCPort` (26357). Check `initCluster` in `internal/docker/cluster.go`
to confirm the correct port constant is used.

---

## Health check: /health not /_admin/v1/health

`/_admin/v1/health` returns 404. The correct liveness endpoint is `/health`.

Pre-init: `/health` returns 200 quickly (HTTP server up before SQL/RPC are ready).
Pre-init: SQL port accepts TCP but closes immediately — don't use it for readiness.

**Dual readiness check in `waitForNodes`:**
1. `GET http://localhost:{HTTPPort}/health` → any HTTP response
2. TCP dial `localhost:{RPCPort}` → can connect

Both must pass before calling `cockroach init`.

---

## Chaos status/clear must auto-discover proxies

`chaos status` and `chaos clear --all` call `GET /proxies` on the Toxiproxy API
and filter by `crdb-node-` prefix. They do NOT use a `--nodes` flag.
This works correctly for any cluster size.

`chaos inject` targets a specific proxy by name (`crdb-node-N`) — works for any N.

---

## Regional latency toxics

Named `crdb-node-N-latency-regional` (distinct from user-injected `crdb-node-N-latency`).
Both coexist on the same proxy — latencies are additive in Toxiproxy.

`ApplyRegionalLatencies` in `internal/docker/chaos.go` calls `chaos.InjectNamedLatency`.
`chaos.WellKnownLatencies` map contains the values:
- us-east: 39ms ±5ms
- us-west: 51ms ±8ms
- eu-central: 57ms ±10ms

`chaos clear` removes ALL toxics including regional ones.
`cdbct chaos regional` re-applies only the regional baselines.

---

## Key files

- `internal/docker/chaos.go` — Toxiproxy container, proxy registration, regional latencies
- `internal/docker/cluster.go` — CRDB node startup, waitForNodes, initCluster
- `internal/chaos/client.go` — Toxiproxy API client (inject, clear, status)
- `cmd/chaos.go` — CLI commands
