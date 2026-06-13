---
name: colima-macos-docker
description: macOS-specific Docker/Colima behaviors and resource requirements for this stack
metadata: 
  node_type: memory
  type: project
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

## Port binding: IPv6 refuses, IPv4 succeeds — not an error

On macOS with Colima, `nc localhost <port>` produces two lines:
```
nc: connectx to localhost port 26257 (tcp) failed: Connection refused   ← IPv6 [::1]
Connection to localhost port 26257 [tcp/*] succeeded!                   ← IPv4 127.0.0.1
```

This is normal. macOS resolves `localhost` to IPv6 first, falls back to IPv4. The port IS accessible — use `127.0.0.1:port` explicitly in connection strings if you need to avoid the IPv6 attempt, or just ignore the refused line.

## host.docker.internal works for container→host communication

From inside a Docker container running in Colima, `host.docker.internal` resolves to the host machine's IP. Used for Prometheus to scrape the workload metrics if the workload were running on the host (it doesn't now — workload is containerized and uses container hostname).

## Colima resource requirements per cluster size

| Cluster size | CockroachDB RAM | Total stack | Recommended Colima |
|---|---|---|---|
| 3 nodes | ~1.1 GB | ~1.5 GB | 4 CPU / 6 GB |
| 9 nodes (default) | ~3.5 GB | ~4 GB | 8 CPU / 12 GB |

CockroachDB is launched with `--cache=128MiB --max-sql-memory=256MiB` per node (384 MB hard limit). Without these flags, CRDB tries to use 25% of available RAM per node — on a 12 GB VM with 9 nodes that's ~3 GB per node × 9 = 27 GB (impossible).

Other containers: Prometheus ~100 MB, Grafana ~150 MB, Toxiproxy ~10 MB, workload ~100 MB.

## Clock drift causes CockroachDB restart loops

Colima's Lima VM has clock drift issues — the system clock occasionally jumps backward. CockroachDB uses HLC (Hybrid Logical Clock) and logs `backward time jump detected (-0.265 seconds)` when this happens. Frequent backward jumps can destabilize the cluster (liveness heartbeat failures, Raft leader elections).

Symptoms: containers show 30-40 second uptime repeatedly, `docker inspect` shows `ExitCode: 0` (clean exit), memory is not OOM killed.

Mitigation:
- Sufficient CPU so the VM isn't CPU-starved (which worsens drift)
- `--cache` and `--max-sql-memory` limits reduce memory pressure (reduces CPU thrash)

## Docker API stalls under memory pressure

When the Colima VM is at >80% memory usage, Docker API calls (container create, exec, etc.) may stall for 10-30 seconds or timeout. This manifests as the CLI appearing to hang. Fix: increase Colima memory allocation.

## Colima restart to change resources

```bash
colima stop
colima start --cpu 8 --memory 12
```

Colima does not support resizing a running VM — must stop and restart. All Docker containers are lost (volumes persist unless `--disk` was set differently).

## Port conflicts with existing services

Default CockroachDB SQL port 26257 may conflict with a locally-running CockroachDB instance. The offset scheme (26257 + node-1) for nodes 1-9 uses ports 26257-26265. Check for conflicts before starting the cluster if running other databases locally.

## Docker exec output is multiplexed

When using `ContainerExecAttach`, the response stream uses Docker's multiplexing protocol (8-byte headers per stdout/stderr frame). Use `stdcopy.StdCopy` to demux — see [[go-docker-sdk-patterns]].

## Colima vs Docker Desktop

This project is tested against Colima, not Docker Desktop. The networking behavior (especially `host.docker.internal` and port binding) may differ slightly with Docker Desktop. The port mapping approach (explicit `0.0.0.0` bindings) works with both.
