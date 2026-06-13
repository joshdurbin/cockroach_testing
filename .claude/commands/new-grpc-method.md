# Adding a gRPC Method to TenantService

Use this when adding a method to `proto/cdbct/v1/tenant.proto`.

---

## Steps

### 1. Define the method and messages in the proto

```proto
// In service TenantService { ... }
rpc MyNewMethod(MyNewMethodRequest) returns (MyNewMethodResponse);

// At the bottom of the file
message MyNewMethodRequest {
  string some_param = 1;
}

message MyNewMethodResponse {
  string result = 1;
  int64  count  = 2;
}
```

Field numbering: never reuse a field number. int64 serializes as JSON string in
proto3 — this is correct behavior, not a bug.

### 2. Regenerate Go stubs

```bash
make proto
# equivalent to: PATH="$PATH:$HOME/go/bin" buf generate
```

This writes to `internal/gen/cdbct/v1/`. The generated files are committed —
do NOT add them to .gitignore.

Requirements: `protoc-gen-go` and `protoc-gen-go-grpc` must be in `~/go/bin/`:
```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

### 3. Implement the handler in grpc_server.go

```go
// In internal/workload/grpc_server.go
func (s *tenantServer) MyNewMethod(ctx context.Context, req *pb.MyNewMethodRequest) (*pb.MyNewMethodResponse, error) {
    // Use s.adminPool for root/admin queries (bypasses RLS)
    // Use s.appPool  for appuser queries (subject to RLS, needs SET LOCAL)
    // Use s.tenants  for in-memory tenant pool (zero DB cost)
    ...
    return &pb.MyNewMethodResponse{...}, nil
}
```

`buf generate` with `require_unimplemented_servers=false` means you won't get
a compile error for unimplemented methods — but the method WON'T appear in
grpcurl until you implement it.

### 4. Test with grpcurl

```bash
# Verify the method appears in reflection
grpcurl -plaintext localhost:9092 describe cdbct.v1.TenantService

# Call the method
grpcurl -plaintext localhost:9092 cdbct.v1.TenantService/MyNewMethod

# With request data
grpcurl -plaintext -d '{"some_param":"value"}' localhost:9092 cdbct.v1.TenantService/MyNewMethod
```

The workload container must be running (`cdbct workload start` or part of `cdbct quickstart`).
The gRPC server starts after both pools and the tenant pool are ready — there's
a few-second delay after container start before methods are callable.

### 5. Document in README

Add the method to the `### Service: cdbct.v1.TenantService` section with:
- Description of what it returns
- The grpcurl call
- An example JSON response

---

## Pool usage guide

| Pool | User | RLS | Use for |
|---|---|---|---|
| `s.adminPool` | root | bypassed | cross-tenant queries, event_counters, tenant table |
| `s.appPool` | appuser | enforced | per-tenant queries with `SET LOCAL app.current_tenant` |
| `s.tenants` | in-memory | n/a | tenant list, tier/region distribution (zero DB cost) |

For parallel queries use `errgroup`:
```go
g, gctx := errgroup.WithContext(ctx)
g.Go(func() error { /* query 1 */ })
g.Go(func() error { /* query 2 */ })
if err := g.Wait(); err != nil { return nil, err }
```

---

## Key files

- `proto/cdbct/v1/tenant.proto` — service + message definitions
- `internal/gen/cdbct/v1/` — generated stubs (do not edit)
- `internal/workload/grpc_server.go` — all handler implementations
- `buf.gen.yaml` — generation config (local plugins)
- `buf.yaml` — module config
