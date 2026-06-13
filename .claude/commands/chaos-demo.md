# Chaos Demo Walkthrough

The canonical demonstration of geo-partition resilience and multi-tenant impact.
Run this after `cdbct quickstart` with ~2 minutes for zone configs to propagate.

---

## Prerequisites

```bash
# Verify the cluster is healthy and zone configs have propagated
cdbct cluster status
# All 9 nodes should show is_available=true, is_live=true

# Verify regional event distribution (should be ~equal across regions)
grpcurl -plaintext localhost:9092 cdbct.v1.TenantService/GetRegionStatus

# Check regional latency baselines are active
cdbct chaos status
# Should show: crdb-node-N [latency(Xms ±Yms)] for all 9 nodes
```

---

## Demo 1: Single-node partition — cluster survives

```bash
# Partition ONE node in us-east (nodes 1, 4, 7)
cdbct chaos inject partition 1

# Watch GetRegionStatus — east event count keeps growing (2/3 replicas remain)
grpcurl -plaintext localhost:9092 cdbct.v1.TenantService/GetRegionStatus

# Check chaos status — proxy disabled
cdbct chaos status

# Grafana: write rates stay flat for all targets (global, east, west, eu)
# ranges_unavailable stays 0 — quorum maintained

# Recover
cdbct chaos clear
cdbct chaos regional   # re-apply regional baselines after clear
```

**What to show:** East partition survives losing 1 of 3 nodes. CockroachDB's
geo-redundancy (3 replicas per region) means a single node failure is invisible
to the workload.

---

## Demo 2: Two-node partition — regional partition fails

```bash
# Partition TWO nodes in us-east (loses quorum: 1/3 replicas remain)
cdbct chaos inject partition 1
cdbct chaos inject partition 4

# GetRegionStatus — east event count STOPS growing
grpcurl -plaintext localhost:9092 cdbct.v1.TenantService/GetRegionStatus

# Who is affected?
grpcurl -plaintext -d '{"region":"us-east"}' localhost:9092 cdbct.v1.TenantService/ListTenantsByRegion

# West and eu continue unaffected
# Grafana: east write rate → 0, errors spike; west/eu write rates unchanged
# ranges_unavailable > 0

# Recover
cdbct chaos clear
cdbct chaos regional
```

**What to show:** Only east-homed tenants' regional writes fail. West and eu-homed
tenants are completely unaffected because their data lives in different partitions.
`ListTenantsByRegion` tells you which customer SLAs are impacted.

---

## Demo 3: Latency injection + admission control divergence

```bash
# Add extra latency on top of regional baseline
cdbct chaos inject latency 2 --latency=500   # us-west gets +500ms (now ~551ms)

# Grafana: watch write latency p99 by QoS tier diverge
# critical tier: stable; background tier: climbs

# Check status — both regional baseline AND injected latency are active
cdbct chaos status
# crdb-node-2 [latency(51ms ±8ms), latency(500ms)]

# Clear injected only — to clear all:
cdbct chaos clear 2       # clears node 2 only (including regional baseline)
cdbct chaos regional      # re-apply regional baselines
```

---

## Demo 4: VerifyRLS during chaos

```bash
# With east partitioned, verify RLS still works for unaffected tenants
grpcurl -plaintext localhost:9092 cdbct.v1.TenantService/VerifyRLS

# Each sample shows rlsEnforced: true
# us-east tenant's visibleRows may be 0 if their data is unavailable
# us-west and eu-central tenants show correct isolation
```

---

## Full cleanup

```bash
cdbct chaos clear     # remove all faults
cdbct chaos regional  # restore regional baselines
cdbct chaos status    # verify clean state
```

---

## Grafana panels to watch

Open http://localhost:3000 → "cdbct — Geo-Partition Resilience + Multi-Tenant"

- **Write Rate by Target** — east flatlines during partition
- **Write Errors by Target** — east spikes
- **Write Latency p99 by QoS** — diverges under latency injection
- **Live Nodes** — drops from 9 to 7 after partitioning 2 nodes
- **Unavailable Ranges** — jumps > 0 when east loses quorum
