---
name: prometheus-metrics-design
description: "Metric naming, label design, cardinality constraints, and Grafana integration patterns"
metadata: 
  node_type: memory
  type: project
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

## Label cardinality: never use UUIDs as label values

50 tenant UUIDs as a `tenant_id` label creates 50× the time series per metric. Prometheus stores one time series per unique label combination — high cardinality causes memory exhaustion and slow queries.

**Wrong:**
```go
writesTotal.WithLabelValues(tenantID.String())  // 50 unique values → 50 series
```

**Right:** Use the low-cardinality tier:
```go
writesTotal.WithLabelValues(target, tenant.QoSTier)  // 4 × 3 = 12 series
```

Rule of thumb: label values should have <20 distinct values per metric. Aggregate higher-cardinality dimensions in the DB (e.g. `event_counters` table), not in Prometheus.

## Two-label pattern serves multiple dashboard stories

Using `target` (global/east/west/eu) and `qos` (critical/regular/background) as two independent labels on the same metric family:

```go
writesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
    Name: "cdbct_writes_total",
}, []string{"target", "qos"})
```

In Grafana:
- `sum(rate(cdbct_writes_total{target="east"}[30s]))` → geo-partition resilience story
- `sum(rate(cdbct_writes_total{qos="background"}[30s]))` → admission control story
- Both stories from one metric, zero duplication

## promauto vs manual registration

`promauto` registers metrics at package init time (no `prometheus.MustRegister` call needed):

```go
var writesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
    Name: "cdbct_writes_total",
    Help: "Total writes successfully committed.",
}, []string{"target", "qos"})
```

Use `promauto` for package-level metrics. Use manual registration only when you need to unregister (rare).

## Metric naming convention

Prefix: `cdbct_` (tool name)
Format: `cdbct_<noun>_<verb>_<unit>`

| Metric | Type | Description |
|---|---|---|
| `cdbct_writes_total` | Counter | Successful writes |
| `cdbct_write_errors_total` | Counter | Failed writes |
| `cdbct_write_duration_seconds` | Histogram | Write latency |
| `cdbct_reads_total` | Counter | Successful reads |
| `cdbct_read_errors_total` | Counter | Failed reads |
| `cdbct_read_duration_seconds` | Histogram | Read latency |
| `cdbct_tenant_pool_size` | Gauge | Tenants per tier |

## Histogram bucket selection

Default Prometheus buckets (`prometheus.DefBuckets`) are designed for HTTP APIs (0.005s to 10s). For CockroachDB operations (typically 1-100ms), use finer-grained low-end buckets:

```go
Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5}
```

## Prometheus HTTP server on separate goroutine

```go
func serveMetrics(addr string) {
    mux := http.NewServeMux()
    mux.Handle("/metrics", promhttp.Handler())
    go func() {
        log.Info().Str("addr", addr).Msg("metrics server started")
        http.ListenAndServe(addr, mux)
    }()
}
```

Start before connecting to DB so the metrics endpoint is available immediately even if DB connection takes time.

## Grafana dashboard provisioning via CopyToContainer

Dashboards are embedded in the binary (`//go:embed`) and injected into the Grafana container before it starts:

```
/etc/grafana/provisioning/datasources/datasource.yml  → Prometheus datasource with UID
/etc/grafana/provisioning/dashboards/provider.yml     → dashboard provider config
/etc/grafana/provisioning/dashboards/*.json           → dashboard JSON files
```

Dashboard JSON files sit alongside `provider.yml` in the same directory — this avoids needing to create `/etc/grafana/dashboards/` (which doesn't exist in the Grafana image).

## Datasource UID must match dashboard references

```yaml
# datasource.yml
datasources:
  - name: Prometheus
    uid: cdbct-prometheus   # ← must match dashboard JSON
```

```json
// dashboard JSON panels
"datasource": { "type": "prometheus", "uid": "cdbct-prometheus" }
```

Mismatched UIDs cause panels to show "datasource not found" despite Prometheus being up.

## Anonymous access for demo Grafana

```
GF_AUTH_ANONYMOUS_ENABLED=true
GF_AUTH_ANONYMOUS_ORG_ROLE=Admin
GF_AUTH_DISABLE_LOGIN_FORM=true
GF_ANALYTICS_REPORTING_ENABLED=false
GF_ANALYTICS_CHECK_FOR_UPDATES=false
```

Appropriate for local demo only. Users go directly to dashboards without login.

## Prometheus scrape targets: use container hostnames

Prometheus runs inside `cdbct-net`. Use container hostnames, not host port mappings:

```yaml
# WRONG — localhost resolves to Prometheus container itself
- targets: ["localhost:8080"]

# CORRECT — container DNS on cdbct-net
- targets: ["cdbct-crdb-1:8080", "cdbct-crdb-2:8080"]
```

CockroachDB's HTTP port is always 8080 internally regardless of what port it's mapped to on the host.
