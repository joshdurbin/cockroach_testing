package workload

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

// Metrics carry two labels:
//   target — which table/partition: global | east | west | eu
//   qos    — tenant's QoS tier:     critical | regular | background
//
// This lets Grafana show two independent stories on the same charts:
//   - Geo-partition resilience: filter/group by target
//   - Admission control + multi-tenancy: filter/group by qos

var (
	writesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cdbct_writes_total",
		Help: "Total writes successfully committed.",
	}, []string{"target", "qos"})

	writeErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cdbct_write_errors_total",
		Help: "Total write errors.",
	}, []string{"target", "qos"})

	writeRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cdbct_write_retries_total",
		Help: "Total transaction retries due to serialization failures (SQLSTATE 40001). Spikes indicate contention during chaos or high write concurrency.",
	}, []string{"target", "qos"})

	writeDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cdbct_write_duration_seconds",
		Help:    "Write latency.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"target", "qos"})

	readsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cdbct_reads_total",
		Help: "Total read queries.",
	}, []string{"target", "qos"})

	readErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cdbct_read_errors_total",
		Help: "Total read errors.",
	}, []string{"target", "qos"})

	readDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cdbct_read_duration_seconds",
		Help:    "Read latency.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"target", "qos"})

	tenantPoolSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cdbct_tenant_pool_size",
		Help: "Number of tenants in the pool by QoS tier.",
	}, []string{"qos"})
)

func serveMetrics(addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Info().Str("addr", addr).Msg("metrics server started")
		if err := http.ListenAndServe(addr, mux); err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("metrics server error")
		}
	}()
}
