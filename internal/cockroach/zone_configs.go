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
	name   string
	region string // the cluster region this config targets
	sql    string
}{
	{
		"events_regional east → us-east (3 replicas + local leaseholder)",
		"us-east",
		`ALTER PARTITION east OF TABLE events_regional CONFIGURE ZONE USING
		    num_replicas      = 3,
		    constraints       = '[+region=us-east]',
		    lease_preferences = '[[+region=us-east]]'`,
	},
	{
		"events_regional west → us-west (3 replicas + local leaseholder)",
		"us-west",
		`ALTER PARTITION west OF TABLE events_regional CONFIGURE ZONE USING
		    num_replicas      = 3,
		    constraints       = '[+region=us-west]',
		    lease_preferences = '[[+region=us-west]]'`,
	},
	{
		"events_regional eu → eu-central (3 replicas + local leaseholder)",
		"eu-central",
		`ALTER PARTITION eu OF TABLE events_regional CONFIGURE ZONE USING
		    num_replicas      = 3,
		    constraints       = '[+region=eu-central]',
		    lease_preferences = '[[+region=eu-central]]'`,
	},
}

// ApplyZoneConfigs applies geo-partition zone configurations for the given regions
// with exponential backoff. Only configs targeting a region present in the list
// are applied — for a single-region cluster only the one relevant partition config
// runs; missing regions are simply skipped.
//
// lease_preferences pin leaseholders to the home region so writes and
// leaseholder-reads travel one LAN hop instead of a potential WAN hop.
func ApplyZoneConfigs(ctx context.Context, pool *pgxpool.Pool, regions []string) error {
	active := make(map[string]bool, len(regions))
	for _, r := range regions {
		active[r] = true
	}

	for _, zc := range zoneConfigs {
		if !active[zc.region] {
			log.Debug().Str("zone", zc.name).Str("region", zc.region).Msg("region not in topology, skipping zone config")
			continue
		}
		if err := applyWithRetry(ctx, pool, zc.name, zc.sql); err != nil {
			return err
		}
	}
	log.Info().Strs("regions", regions).Msg("zone configs applied")
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
