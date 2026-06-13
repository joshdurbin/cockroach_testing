---
name: grpc-buf-patterns
description: "gRPC setup with reflection, buf toolchain configuration, and grpcurl/ghz usage"
metadata: 
  node_type: memory
  type: project
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

## Minimum viable gRPC server with reflection

```go
import (
    "google.golang.org/grpc"
    "google.golang.org/grpc/reflection"
    pb "github.com/joshdurbin/cockroach_testing/internal/gen/cdbct/v1"
)

lis, _ := net.Listen("tcp", ":9092")
srv := grpc.NewServer()
pb.RegisterTenantServiceServer(srv, &myServer{})
reflection.Register(srv)   // ← one line enables grpcurl discovery
srv.Serve(lis)
```

With reflection, `grpcurl -plaintext localhost:9092 list` returns all registered services and their methods without needing a .proto file.

## Plain TCP (no TLS) — -plaintext / --insecure

The gRPC server in this project is unencrypted, matching CockroachDB's `--insecure` mode.

```bash
grpcurl --plaintext localhost:9092 list          # grpcurl flag
ghz --insecure --call SvcName.Method localhost:9092  # ghz flag
```

Without `-plaintext`, both tools attempt TLS and fail with "first record does not look like a TLS handshake". This is the most common gotcha.

## buf toolchain: local plugins are more reliable

`buf.gen.yaml` with local plugins (uses $PATH binaries, no network):
```yaml
version: v2
plugins:
  - local: protoc-gen-go
    out: internal/gen
    opt:
      - paths=source_relative
  - local: protoc-gen-go-grpc
    out: internal/gen
    opt:
      - paths=source_relative
      - require_unimplemented_servers=false
```

Install the plugins once:
```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

Remote buf.build plugins (`remote: buf.build/protocolbuffers/go`) require internet at generate time and have version pinning complexity. Local plugins are simpler for a single-developer tool.

## buf.yaml for a simple proto layout

```yaml
version: v2
modules:
  - path: proto
deps:
  - buf.build/googleapis/googleapis  # only needed if importing google/protobuf/timestamp.proto
```

Run `buf dep update` after changing deps to update `buf.lock`.

## Proto organization convention

```
proto/
  cdbct/
    v1/
      tenant.proto   # service + all messages in one file for small services
```

Generated output goes to `internal/gen/cdbct/v1/` and IS committed to the repo. This avoids requiring buf/protoc in CI and makes the generated interface visible in code review.

`go_package` in the proto:
```proto
option go_package = "github.com/joshdurbin/cockroach_testing/internal/gen/cdbct/v1;cdbctv1";
```

## `require_unimplemented_servers=false`

Without this, protoc-gen-go-grpc embeds `mustEmbedUnimplementedXxxServer()` which forces all server implementations to embed `UnimplementedXxxServer`. Setting `require_unimplemented_servers=false` removes that requirement, making it easier to implement the interface directly.

## Makefile target for regeneration

```makefile
proto:
    PATH="$$PATH:$$HOME/go/bin" buf generate
```

The PATH extension ensures the local protoc-gen-go and protoc-gen-go-grpc binaries (in `~/go/bin/`) are found.

## grpcurl command reference for this project

Five methods on cdbct.v1.TenantService:

```bash
# Discovery
grpcurl -plaintext localhost:9092 list
grpcurl -plaintext localhost:9092 describe cdbct.v1.TenantService

# Calls — all five methods
grpcurl -plaintext localhost:9092 cdbct.v1.TenantService/ListTenants
grpcurl -plaintext localhost:9092 cdbct.v1.TenantService/VerifyRLS
grpcurl -plaintext -d '{"tenant_id":"<uuid>"}' localhost:9092 cdbct.v1.TenantService/GetTenantEvents
grpcurl -plaintext localhost:9092 cdbct.v1.TenantService/GetRegionStatus
grpcurl -plaintext -d '{"region":"us-east"}' localhost:9092 cdbct.v1.TenantService/ListTenantsByRegion
```

`GetRegionStatus` is the most operationally useful during chaos — shows per-region tenant count and event count so you know exactly which tenants are in an impacted partition.

## ghz load test reference for this project

```bash
# 10 RPS baseline
ghz --insecure --call cdbct.v1.TenantService.ListTenants --rps 10 -z 30s localhost:9092

# Concurrency burst
ghz --insecure --call cdbct.v1.TenantService.ListTenants -c 50 --total 500 localhost:9092

# With request data
ghz --insecure --call cdbct.v1.TenantService.GetTenantEvents \
    --data '{"tenant_id":"<uuid>"}' --rps 20 -z 30s localhost:9092

# Saturation (observe admission control)
ghz --insecure --call cdbct.v1.TenantService.ListTenants --rps 100 -z 30s --timeout 10s localhost:9092
```

## int64 in proto JSON response

Proto3 serializes `int64` fields as JSON strings (not numbers) to preserve precision for JavaScript clients. `ghz` and `grpcurl` show `"totalEvents": "83100"` not `83100`. This is spec-correct, not a bug.

## gRPC port assignment

- `:9091` — Prometheus metrics HTTP (existing)
- `:9092` — gRPC (new)
- Both are exposed from the workload container and mapped to the same host ports
