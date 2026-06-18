package workload

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/joshdurbin/cockroach_testing/internal/db"
)

const (
	QoSCritical   = "critical"
	QoSRegular    = "regular"
	QoSBackground = "background"
)

// Tenant represents one entry in the fixed tenant pool.
type Tenant struct {
	ID         uuid.UUID
	QoSTier    string
	HomeRegion string // the region whose partition holds this tenant's data
}

// TenantPool is the in-memory tenant pool loaded from the tenants table.
type TenantPool struct {
	mu       sync.RWMutex
	all      []Tenant
	byTier   map[string][]Tenant
	byRegion map[string][]Tenant
}

// Random returns a uniformly random tenant from the pool.
func (p *TenantPool) Random() Tenant {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.all[rand.IntN(len(p.all))]
}

// Len returns the total number of tenants.
func (p *TenantPool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.all)
}

// TierCount returns the number of tenants in a given QoS tier.
func (p *TenantPool) TierCount(tier string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.byTier[tier])
}

// ByRegion returns a copy of the tenants homed to a region.
func (p *TenantPool) ByRegion(region string) []Tenant {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Tenant, len(p.byRegion[region]))
	copy(out, p.byRegion[region])
	return out
}

// All returns a copy of all tenants.
func (p *TenantPool) All() []Tenant {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Tenant, len(p.all))
	copy(out, p.all)
	return out
}

// ActiveRegions returns the sorted list of regions that have at least one tenant.
func (p *TenantPool) ActiveRegions() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	regions := make([]string, 0, len(p.byRegion))
	for r, tenants := range p.byRegion {
		if len(tenants) > 0 {
			regions = append(regions, r)
		}
	}
	sort.Strings(regions)
	return regions
}

// SeedOrLoad seeds the tenants table with n tenant UUIDs if it is empty,
// or loads the existing pool if already seeded.
//
// QoS distribution: 20% critical, 60% regular, 20% background (min 1 each).
// Region distribution: cyclic over the provided regions list.
// These two dimensions are assigned independently so every combination is
// represented as the pool grows.
func SeedOrLoad(ctx context.Context, adminPool *pgxpool.Pool, n int, regions []string) (*TenantPool, error) {
	q := db.New(adminPool)

	count, err := q.CountTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("count tenants: %w", err)
	}

	if count == 0 {
		if err := seed(ctx, q, n, regions); err != nil {
			return nil, fmt.Errorf("seed tenants: %w", err)
		}
		log.Info().Int("count", n).Strs("regions", regions).Msg("tenant pool seeded")
	} else {
		log.Info().Int("count", int(count)).Msg("tenant pool already exists, loading")
	}

	return load(ctx, q)
}

func seed(ctx context.Context, q *db.Queries, n int, regions []string) error {
	critical, regular, background := distribute(n)

	// Build the tier list.
	tiers := make([]string, 0, n)
	for range critical {
		tiers = append(tiers, QoSCritical)
	}
	for range regular {
		tiers = append(tiers, QoSRegular)
	}
	for range background {
		tiers = append(tiers, QoSBackground)
	}

	// Assign home_region cyclically, independent of QoS tier, so every
	// region × tier combination is represented as the pool grows.
	for i, tier := range tiers {
		homeRegion := regions[i%len(regions)]
		if err := q.InsertTenant(ctx, db.InsertTenantParams{
			ID:         uuid.New(),
			QosTier:    tier,
			HomeRegion: homeRegion,
		}); err != nil {
			return err
		}
	}
	return nil
}

func load(ctx context.Context, q *db.Queries) (*TenantPool, error) {
	rows, err := q.LoadTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("load tenants: %w", err)
	}
	p := &TenantPool{
		all:      make([]Tenant, 0, len(rows)),
		byTier:   make(map[string][]Tenant),
		byRegion: make(map[string][]Tenant),
	}
	for _, r := range rows {
		t := Tenant{ID: r.ID, QoSTier: r.QosTier, HomeRegion: r.HomeRegion}
		p.all = append(p.all, t)
		p.byTier[r.QosTier] = append(p.byTier[r.QosTier], t)
		p.byRegion[r.HomeRegion] = append(p.byRegion[r.HomeRegion], t)
	}
	log.Info().
		Int("total", len(p.all)).
		Int("critical", len(p.byTier[QoSCritical])).
		Int("regular", len(p.byTier[QoSRegular])).
		Int("background", len(p.byTier[QoSBackground])).
		Strs("regions", p.ActiveRegions()).
		Msg("tenant pool loaded")
	return p, nil
}

// distribute splits n tenants into critical/regular/background (20/60/20).
func distribute(n int) (critical, regular, background int) {
	critical = max(1, int(float64(n)*0.20+0.5))
	background = max(1, int(float64(n)*0.20+0.5))
	regular = n - critical - background
	if regular < 1 {
		regular = 1
		if critical > 1 {
			critical--
		} else {
			background--
		}
	}
	return
}

// AppUserDSN derives the appuser DSN from the admin (root) DSN.
func AppUserDSN(adminDSN string) string {
	for i := range len(adminDSN) - 3 {
		if adminDSN[i:i+3] == "://" {
			rest := adminDSN[i+3:]
			if at := indexByte(rest, '@'); at >= 0 {
				return adminDSN[:i+3] + "appuser@" + rest[at+1:]
			}
		}
	}
	return adminDSN
}

func indexByte(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}
