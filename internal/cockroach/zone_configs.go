package cockroach

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// zoneConfigs contains the CONFIGURE ZONE statements for geo-partitioned tables.
// These are applied after migrations because CONFIGURE ZONE fails on a freshly
// initialized cluster while the zone subsystem is still starting up.
// All statements are idempotent — safe to run on every startup.
var zoneConfigs = []struct {
	name string
	sql  string
}{
	{
		"events_regional east → us-east (3 replicas + local leaseholder)",
		`ALTER PARTITION east OF TABLE events_regional CONFIGURE ZONE USING
		    num_replicas      = 3,
		    constraints       = '[+region=us-east]',
		    lease_preferences = '[[+region=us-east]]'`,
	},
	{
		"events_regional west → us-west (3 replicas + local leaseholder)",
		`ALTER PARTITION west OF TABLE events_regional CONFIGURE ZONE USING
		    num_replicas      = 3,
		    constraints       = '[+region=us-west]',
		    lease_preferences = '[[+region=us-west]]'`,
	},
	{
		"events_regional eu → eu-central (3 replicas + local leaseholder)",
		`ALTER PARTITION eu OF TABLE events_regional CONFIGURE ZONE USING
		    num_replicas      = 3,
		    constraints       = '[+region=eu-central]',
		    lease_preferences = '[[+region=eu-central]]'`,
	},
}

// ApplyZoneConfigs applies geo-partition zone configurations with exponential
// backoff. The zone subsystem on a freshly initialized cluster may not be
// ready immediately after cockroach init, so retries are expected on first run.
//
// lease_preferences pin leaseholders to the home region so writes and
// leaseholder-reads travel one LAN hop instead of a potential WAN hop.
// Without this, leaseholders drift freely after rebalancing even when
// constraints keep replicas in the correct region.
func ApplyZoneConfigs(ctx context.Context, pool *pgxpool.Pool) error {
	for _, zc := range zoneConfigs {
		if err := applyWithRetry(ctx, pool, zc.name, zc.sql); err != nil {
			return err
		}
	}
	log.Info().Msg("zone configs applied")
	return nil
}

func applyWithRetry(ctx context.Context, pool *pgxpool.Pool, name, sql string) error {
	backoff := time.Second
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, err := pool.Exec(ctx, sql)
		if err == nil {
			log.Debug().Str("zone", name).Msg("zone config applied")
			return nil
		}
		log.Debug().Err(err).Str("zone", name).Dur("retry_in", backoff).Msg("zone config failed, retrying")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
	// Zone configs failing is non-fatal — the cluster works without them,
	// only the geo-partition availability demo is affected.
	log.Warn().Str("zone", name).Msg("zone config did not apply within 60s — geo-partition demo may not show correct availability")
	return nil
}
