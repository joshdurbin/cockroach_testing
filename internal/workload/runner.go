package workload

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/brianvoe/gofakeit/v7"
	crdbpgxv5 "github.com/cockroachdb/cockroach-go/v2/crdb/crdbpgxv5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/joshdurbin/cockroach_testing/internal/cockroach"
	"github.com/joshdurbin/cockroach_testing/internal/db"
)

// Config controls the workload runner.
type Config struct {
	DSN          string            // admin (root) DSN — used for migrations and seeding
	RegionalDSNs map[string]string // region → root DSN scoped to that region's nodes
	Regions      []string          // active regions; derived from RegionalDSNs if empty
	Interval     time.Duration     // tick interval between write rounds
	BatchSize    int               // writes per tick (each write = global + home-region)
	QueryEvery   time.Duration     // how often to run read queries
	TenantCount  int               // number of tenants to seed
	MetricsAddr  string            // Prometheus metrics HTTP listen address
	GRPCAddr     string            // gRPC server listen address
}

var (
	eventTypes = []string{"login", "logout", "access", "create", "modify", "delete", "error", "warning"}
	severities = []string{"info", "info", "info", "info", "warning", "error"}
)

// Run connects, migrates, seeds, starts gRPC, then drives a tick-based loop.
func Run(ctx context.Context, cfg Config) error {
	serveMetrics(cfg.MetricsAddr)

	// Derive active regions from non-empty regional DSNs when not explicitly set.
	if len(cfg.Regions) == 0 {
		for region, dsn := range cfg.RegionalDSNs {
			if dsn != "" {
				cfg.Regions = append(cfg.Regions, region)
			}
		}
		sort.Strings(cfg.Regions)
	}
	// Fall back to the full three-region set if no regional DSNs were provided.
	if len(cfg.Regions) == 0 {
		cfg.Regions = []string{"us-east", "us-west", "eu-central"}
	}

	// Admin pool: root — runs migrations, seeds tenants, used by gRPC admin queries.
	adminPool, err := cockroach.Connect(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("admin connect: %w", err)
	}
	defer adminPool.Close()

	if err := cockroach.ApplyZoneConfigs(ctx, adminPool.Pool, cfg.Regions); err != nil {
		return fmt.Errorf("zone configs: %w", err)
	}

	tenants, err := SeedOrLoad(ctx, adminPool.Pool, cfg.TenantCount, cfg.Regions)
	if err != nil {
		return fmt.Errorf("tenant pool: %w", err)
	}

	tenantPoolSize.WithLabelValues(QoSCritical).Set(float64(tenants.TierCount(QoSCritical)))
	tenantPoolSize.WithLabelValues(QoSRegular).Set(float64(tenants.TierCount(QoSRegular)))
	tenantPoolSize.WithLabelValues(QoSBackground).Set(float64(tenants.TierCount(QoSBackground)))

	// Appuser pool: subject to RLS — fallback for regions with no dedicated pool.
	appPool, err := cockroach.ConnectRaw(ctx, AppUserDSN(cfg.DSN))
	if err != nil {
		return fmt.Errorf("appuser connect: %w", err)
	}
	defer appPool.Close()

	// Per-region appuser pools: route home-region writes through nodes local to the
	// tenant's region, eliminating cross-region SQL gateway hops before the write
	// reaches the leaseholder (which lease_preferences pins to the same region).
	regionalPools := make(map[string]*pgxpool.Pool, len(cfg.RegionalDSNs))
	for region, rdsn := range cfg.RegionalDSNs {
		if rdsn == "" {
			continue
		}
		rpool, err := cockroach.ConnectRaw(ctx, AppUserDSN(rdsn))
		if err != nil {
			return fmt.Errorf("appuser connect (%s): %w", region, err)
		}
		defer rpool.Close()
		regionalPools[region] = rpool
	}
	log.Info().
		Int("regional_pools", len(regionalPools)).
		Strs("regions", cfg.Regions).
		Msg("per-region pools connected")

	go func() {
		if err := ServeGRPC(cfg.GRPCAddr, tenants, adminPool.Pool, appPool); err != nil {
			log.Error().Err(err).Msg("gRPC server error")
		}
	}()

	log.Info().
		Dur("interval", cfg.Interval).
		Int("batch", cfg.BatchSize).
		Int("tenants", tenants.Len()).
		Strs("regions", cfg.Regions).
		Msg("workload running")

	writeTicker := time.NewTicker(cfg.Interval)
	defer writeTicker.Stop()
	queryTicker := time.NewTicker(cfg.QueryEvery)
	defer queryTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-writeTicker.C:
			go runWrites(ctx, appPool, regionalPools, tenants, cfg.BatchSize)
		case <-queryTicker.C:
			go runReads(ctx, appPool, regionalPools, tenants, cfg.Regions)
		}
	}
}

// runWrites performs one write round: for each item in the batch, pick a random
// tenant and issue two concurrent writes:
//
//  1. events (resilient, 3 replicas balanced) — uses the shared appPool since
//     the global table has no home-region affinity.
//  2. events_regional (geo-partitioned) — uses the home-region pool to route
//     through a local SQL gateway node, then one LAN hop to the leaseholder.
func runWrites(ctx context.Context, appPool *pgxpool.Pool, regionalPools map[string]*pgxpool.Pool, tenants *TenantPool, batch int) {
	var wg sync.WaitGroup
	for range batch {
		t := tenants.Random()
		homeTarget := regionTarget(t.HomeRegion)

		// Resolve the home-region pool; fall back to appPool if none configured.
		homePool := regionalPools[t.HomeRegion]
		if homePool == nil {
			homePool = appPool
		}

		// Write 1: events — geo-tagged to tenant's home region.
		wg.Add(1)
		go func(tenant Tenant) {
			defer wg.Done()
			recordWrite("global", tenant, func() error {
				return tenantInsert(ctx, appPool, tenant, "global", func(qtx *db.Queries) error {
					return qtx.InsertEvent(ctx, db.InsertEventParams{
						TenantID:  pgtype.UUID{Bytes: tenant.ID, Valid: true},
						Region:    tenant.HomeRegion,
						EventType: pick(eventTypes),
						Actor:     gofakeit.Username(),
						Severity:  pick(severities),
						Payload:   eventPayload(),
					})
				})
			})
		}(t)

		// Write 2: events_regional — routed through home-region nodes.
		wg.Add(1)
		go func(tenant Tenant, target string, pool *pgxpool.Pool) {
			defer wg.Done()
			recordWrite(target, tenant, func() error {
				return tenantInsert(ctx, pool, tenant, target, func(qtx *db.Queries) error {
					return qtx.InsertRegionalEvent(ctx, db.InsertRegionalEventParams{
						TenantID:  pgtype.UUID{Bytes: tenant.ID, Valid: true},
						Region:    tenant.HomeRegion,
						EventType: pick(eventTypes),
						Actor:     gofakeit.Username(),
						Severity:  pick(severities),
						Payload:   eventPayload(),
					})
				})
			})
		}(t, homeTarget, homePool)
	}
	wg.Wait()
}

func recordWrite(target string, t Tenant, fn func() error) {
	start := time.Now()
	err := fn()
	dur := time.Since(start)
	writeDuration.WithLabelValues(target, t.QoSTier).Observe(dur.Seconds())
	if err != nil {
		writeErrors.WithLabelValues(target, t.QoSTier).Inc()
		log.Debug().Err(err).Str("target", target).Str("qos", t.QoSTier).Msg("write error")
	} else {
		writesTotal.WithLabelValues(target, t.QoSTier).Inc()
	}
}

// tenantInsert wraps a database write in a transaction that sets tenant context,
// QoS tier, runs the insert, and increments the write-side counter atomically.
//
// Retries use CockroachDB's SAVEPOINT cockroach_restart protocol via the
// official cockroach-go library. On a serialization failure the server can
// restart the transaction without a new network round-trip, which is more
// efficient than rolling back and re-beginning from the client.
func tenantInsert(ctx context.Context, pool *pgxpool.Pool, t Tenant, target string, fn func(*db.Queries) error) error {
	attempts := 0
	return crdbpgxv5.ExecuteTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if attempts > 0 {
			writeRetries.WithLabelValues(target, t.QoSTier).Inc()
		}
		attempts++
		if err := setTenantContext(ctx, tx, t); err != nil {
			return err
		}
		q := db.New(tx)
		if err := fn(q); err != nil {
			return err
		}
		return q.IncrementCounter(ctx, target)
	})
}

func setTenantContext(ctx context.Context, tx pgx.Tx, t Tenant) error {
	if _, err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", t.ID.String()); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL default_transaction_quality_of_service = $1", t.QoSTier); err != nil {
		return fmt.Errorf("set qos: %w", err)
	}
	return nil
}

// runReads queries recent event counts for a randomly sampled tenant from each
// active region, exercising every partition's read path each tick.
//
// QoS differentiation:
//   - critical / regular — strong consistent reads (leaseholder-served).
//   - background         — follower reads via AS OF SYSTEM TIME follower_read_timestamp().
//     Served from the nearest replica regardless of leaseholder location.
//     Tolerable staleness (typically ~3s) is acceptable for background-tier tenants
//     and makes the latency gap between tiers visible even at low write rates.
func runReads(ctx context.Context, appPool *pgxpool.Pool, regionalPools map[string]*pgxpool.Pool, tenants *TenantPool, regions []string) {
	since := pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}

	read := func(target string, tenant Tenant, fn func() error) {
		start := time.Now()
		err := fn()
		readDuration.WithLabelValues(target, tenant.QoSTier).Observe(time.Since(start).Seconds())
		if err != nil {
			readErrors.WithLabelValues(target, tenant.QoSTier).Inc()
			log.Debug().Err(err).Str("target", target).Msg("read error")
		} else {
			readsTotal.WithLabelValues(target, tenant.QoSTier).Inc()
		}
	}

	for _, region := range regions {
		regional := tenants.ByRegion(region)
		if len(regional) == 0 {
			continue
		}
		tenant := regional[rand.IntN(len(regional))]
		target := regionTarget(region)

		// Use home-region pool for regional reads so the query reaches a local replica.
		rpool := regionalPools[region]
		if rpool == nil {
			rpool = appPool
		}

		read("global", tenant, func() error {
			tx, err := appPool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if err := setTenantContext(ctx, tx, tenant); err != nil {
				return err
			}
			if tenant.QoSTier == QoSBackground {
				if _, err := tx.Exec(ctx, "SET TRANSACTION AS OF SYSTEM TIME follower_read_timestamp()"); err != nil {
					return err
				}
			}
			if _, err := db.New(tx).CountRecentEvents(ctx, since); err != nil {
				return err
			}
			return tx.Commit(ctx)
		})

		read(target, tenant, func() error {
			tx, err := rpool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if err := setTenantContext(ctx, tx, tenant); err != nil {
				return err
			}
			if tenant.QoSTier == QoSBackground {
				if _, err := tx.Exec(ctx, "SET TRANSACTION AS OF SYSTEM TIME follower_read_timestamp()"); err != nil {
					return err
				}
			}
			if _, err := db.New(tx).CountRecentRegionalEvents(ctx, db.CountRecentRegionalEventsParams{
				Region:    tenant.HomeRegion,
				CreatedAt: since,
			}); err != nil {
				return err
			}
			return tx.Commit(ctx)
		})
	}
}

func regionTarget(region string) string {
	switch region {
	case "us-east":
		return "east"
	case "us-west":
		return "west"
	case "eu-central":
		return "eu"
	default:
		return region
	}
}

func pick(s []string) string { return s[rand.IntN(len(s))] }

func eventPayload() []byte {
	b, _ := json.Marshal(map[string]any{
		"ip":         gofakeit.IPv4Address(),
		"user_agent": gofakeit.UserAgent(),
		"request_id": uuid.New().String(),
	})
	return b
}
