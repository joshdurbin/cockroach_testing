# Debugging Common Cluster Failures

Reference for the three most common failure modes in this project.

---

## Failure 1: Cluster won't form / nodes can't gossip

**Symptom:** `waitForNodes` times out, or `cockroach init` fails, or nodes show
"unable to contact other nodes" in `docker logs`.

**Diagnosis:**
```bash
docker logs cdbct-toxiproxy | grep -E "Accepted|terminated|error"
# Should see: "Accepted client" for each node connecting through Toxiproxy
# If no Accepted lines → proxies not registered

curl -s http://localhost:8474/proxies | python3 -m json.tool | grep name
# Should list crdb-node-1 through crdb-node-9
# If empty → EnsureToxiproxy ran but registerToxiproxyProxies failed
```

**Root cause:** Toxiproxy container started but proxy rules not registered before
nodes tried to connect. See `/toxiproxy-wiring`.

**Fix:** `cdbct destroy && cdbct quickstart` — the fixed code registers proxies
before starting nodes.

---

## Failure 2: Workload can't connect / "unexpected EOF"

**Symptom:** Workload container logs show "failed to receive message: unexpected EOF"
or times out on first connection.

**Diagnosis:**
```bash
# Check if SQL port is accepting connections (IPv4)
nc -zv 127.0.0.1 26257
# "succeeded" = port open; "refused" = container not running

# Check inside container (always use internal port 26257, not host-mapped port)
docker exec cdbct-crdb-1 cockroach sql --insecure --host=localhost:26257 \
  --execute="SELECT version()"
```

**Common causes:**

1. **Node just restarted** — CockroachDB accepts TCP but closes it while the SQL
   server is still initializing after restart. Wait 10-15s and retry. The workload
   has 60s ping retry with exponential backoff — it will self-heal.

2. **Wrong port in Exec commands** — Inside the container, SQL port is always 26257.
   `n.SQLPort` is the HOST-mapped port (26258, 26259...) which doesn't exist inside
   the container. Use `CRDBInternalSQLPort` (26257) for any `m.Exec()` call.

3. **Memory pressure** — 9 nodes without `--cache=128MiB --max-sql-memory=256MiB`
   exhaust Colima's RAM, causing restart loops. Check `docker stats`.

---

## Failure 3: Migrations slow or failing

**Symptom:** "running database migrations" takes >15 seconds, or migration fails
with "relation X does not exist".

**Diagnosis:**
```bash
docker logs <workload-container> | grep -E "migration|goose|ERROR"
```

**Cause A: Regional latency injected before migrations ran**

Schema changes require Raft consensus — with 57ms proxy latency each DDL
statement takes 4-6s. 20 statements × 5s = 100s.

Fix: `cdbct chaos clear` to remove latencies temporarily, restart workload,
then `cdbct chaos regional` after migrations complete.

Long-term fix: run migrations from quickstart before calling `ApplyRegionalLatencies`
(see the rejected commit that was backed out — user preferred not to change this).

**Cause B: Stale volumes from old schema**

Goose records migration versions. If volumes survived a schema-breaking change,
goose sees old version N as applied, skips to N+1 which expects tables that
don't exist.

```bash
make docker-clean    # removes ALL volumes
cdbct quickstart
```

**Cause C: ALTER TABLE in a migration**

`ALTER TABLE ADD COLUMN` is slow even on empty tables. Consolidate all columns
into the original `CREATE TABLE` in the earliest migration. See `/new-migration`.

---

## General diagnostic commands

```bash
# Full cluster status (runs cockroach node status inside container)
cdbct cluster status

# Container resource usage
docker stats --no-stream

# Node logs for a specific container
docker logs cdbct-crdb-1 --tail=50

# All CockroachDB log files inside a node
docker exec cdbct-crdb-1 ls /cockroach/cockroach-data/logs/

# Recent warnings/errors in CRDB logs
docker exec cdbct-crdb-1 grep -i "error\|fatal\|backward time" \
  /cockroach/cockroach-data/logs/cockroach.log | tail -20

# Toxiproxy proxy state
curl -s http://localhost:8474/proxies | python3 -m json.tool

# Workload gRPC health
grpcurl -plaintext localhost:9092 cdbct.v1.TenantService/GetRegionStatus

# Check all volumes exist
docker volume ls | grep cdbct
```

---

## Nuclear option

```bash
make docker-clean   # removes everything: containers, images, volumes, cache
cdbct quickstart    # fresh start
```
