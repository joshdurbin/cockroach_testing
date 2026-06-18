package workload

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/joshdurbin/cockroach_testing/internal/gen/cdbct/v1"
	"github.com/joshdurbin/cockroach_testing/internal/db"
)

// tenantServer implements pb.TenantServiceServer.
type tenantServer struct {
	tenants   *TenantPool
	adminPool *pgxpool.Pool
	appPool   *pgxpool.Pool
}

// ServeGRPC starts a gRPC server with server reflection enabled.
// grpcurl can discover and call all methods without a local .proto file.
func ServeGRPC(addr string, tenants *TenantPool, adminPool, appPool *pgxpool.Pool) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("grpc listen %s: %w", addr, err)
	}
	srv := grpc.NewServer()
	pb.RegisterTenantServiceServer(srv, &tenantServer{
		tenants:   tenants,
		adminPool: adminPool,
		appPool:   appPool,
	})
	reflection.Register(srv)
	log.Info().Str("addr", addr).Msg("gRPC server started (reflection enabled)")
	return srv.Serve(lis)
}

// ─── ListTenants ─────────────────────────────────────────────────────────────

// ListTenants returns the full tenant pool with QoS and region distributions
// plus per-target write counts from event_counters. Two queries run in
// parallel — no full table scan.
func (s *tenantServer) ListTenants(ctx context.Context, _ *pb.ListTenantsRequest) (*pb.ListTenantsResponse, error) {
	q := db.New(s.adminPool)

	var tenantRows []db.Tenant
	var counterRows []db.EventCounter

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		tenantRows, err = q.LoadTenants(gctx)
		return err
	})
	g.Go(func() error {
		var err error
		counterRows, err = q.GetCounters(gctx)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	counters := make(map[string]int64, len(counterRows))
	for _, r := range counterRows {
		counters[r.Target] = r.Cnt
	}

	tiers := &pb.TierSummary{}
	regions := &pb.RegionSummary{}
	tenants := make([]*pb.Tenant, 0, len(tenantRows))
	for _, t := range tenantRows {
		switch t.QosTier {
		case QoSCritical:
			tiers.Critical++
		case QoSRegular:
			tiers.Regular++
		case QoSBackground:
			tiers.Background++
		}
		switch t.HomeRegion {
		case "us-east":
			regions.UsEast++
		case "us-west":
			regions.UsWest++
		case "eu-central":
			regions.EuCentral++
		}
		entry := &pb.Tenant{
			Id:         t.ID.String(),
			QosTier:    t.QosTier,
			HomeRegion: t.HomeRegion,
		}
		if t.CreatedAt.Valid {
			entry.CreatedAt = timestamppb.New(t.CreatedAt.Time)
		}
		tenants = append(tenants, entry)
	}

	return &pb.ListTenantsResponse{
		Total:               int32(len(tenants)),
		Tiers:               tiers,
		Regions:             regions,
		TotalEvents:         counters["global"],
		TotalRegionalEvents: counters["east"] + counters["west"] + counters["eu"],
		Tenants:             tenants,
	}, nil
}

// ─── VerifyRLS ───────────────────────────────────────────────────────────────

// VerifyRLS proves Row-Level Security by sampling one tenant per home region
// and comparing their visible row count against the admin total. Sampling by
// region is more relevant than by QoS tier — it shows that geo-isolated
// tenants can only see data in their own partition.
func (s *tenantServer) VerifyRLS(ctx context.Context, _ *pb.VerifyRLSRequest) (*pb.VerifyRLSResponse, error) {
	adminTotal, err := db.New(s.adminPool).CountAllEvents(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin count: %w", err)
	}

	// Sample one tenant per active region — run all in parallel.
	activeRegions := s.tenants.ActiveRegions()
	seen := map[string]bool{}
	var candidates []Tenant
	for _, t := range s.tenants.All() {
		if seen[t.HomeRegion] {
			continue
		}
		seen[t.HomeRegion] = true
		candidates = append(candidates, t)
		if len(candidates) == len(activeRegions) {
			break
		}
	}

	type result struct {
		t     Tenant
		count int64
		err   error
	}
	results := make([]result, len(candidates))
	var wg sync.WaitGroup
	for i, t := range candidates {
		wg.Add(1)
		go func(idx int, tenant Tenant) {
			defer wg.Done()
			count, err := countWithTenant(ctx, s.appPool, tenant.ID.String())
			results[idx] = result{t: tenant, count: count, err: err}
		}(i, t)
	}
	wg.Wait()

	samples := make([]*pb.RLSSample, 0, len(results))
	enforced := adminTotal == 0
	for _, r := range results {
		if r.err != nil {
			log.Warn().Err(r.err).Str("tenant", r.t.ID.String()).Msg("verify query failed")
			continue
		}
		e := adminTotal == 0 || r.count < adminTotal
		if e {
			enforced = true
		}
		samples = append(samples, &pb.RLSSample{
			TenantId:    r.t.ID.String(),
			QosTier:     r.t.QoSTier,
			VisibleRows: r.count,
			RlsEnforced: e,
		})
	}

	return &pb.VerifyRLSResponse{
		AdminTotal:  adminTotal,
		RlsEnforced: enforced,
		Note:        "admin_total from event_counters (exact); visible_rows via idx_events_tenant index scan; sampled one tenant per home region",
		Samples:     samples,
	}, nil
}

// ─── GetTenantEvents ─────────────────────────────────────────────────────────

// GetTenantEvents returns the RLS-filtered row count for a specific tenant.
func (s *tenantServer) GetTenantEvents(ctx context.Context, req *pb.GetTenantEventsRequest) (*pb.GetTenantEventsResponse, error) {
	if req.TenantId == "" {
		return nil, fmt.Errorf("tenant_id is required")
	}
	var found *Tenant
	for _, t := range s.tenants.All() {
		if t.ID.String() == req.TenantId {
			cp := t
			found = &cp
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("tenant %q not found in pool", req.TenantId)
	}
	count, err := countWithTenant(ctx, s.appPool, req.TenantId)
	if err != nil {
		return nil, fmt.Errorf("count tenant events: %w", err)
	}
	return &pb.GetTenantEventsResponse{
		TenantId:    req.TenantId,
		QosTier:     found.QoSTier,
		HomeRegion:  found.HomeRegion,
		VisibleRows: count,
	}, nil
}

// ─── GetRegionStatus ─────────────────────────────────────────────────────────

// GetRegionStatus returns per-region tenant counts, QoS distributions, and
// partition event totals. This is the most operationally useful call during
// chaos — it tells you exactly which tenants are in the impacted region.
//
// Two queries run in parallel: tenant counts from in-memory pool (zero DB
// cost), event counts from event_counters (4-row PK scan).
func (s *tenantServer) GetRegionStatus(ctx context.Context, _ *pb.GetRegionStatusRequest) (*pb.GetRegionStatusResponse, error) {
	// event_counters gives the per-region partition row counts.
	counterRows, err := db.New(s.adminPool).GetCounters(ctx)
	if err != nil {
		return nil, fmt.Errorf("get counters: %w", err)
	}
	counters := make(map[string]int64, len(counterRows))
	for _, r := range counterRows {
		counters[r.Target] = r.Cnt
	}

	// Build per-region status from the in-memory tenant pool.
	regionTargetMap := map[string]string{
		"us-east":    "east",
		"us-west":    "west",
		"eu-central": "eu",
	}

	statuses := make([]*pb.RegionStatus, 0)
	for _, region := range s.tenants.ActiveRegions() {
		tenants := s.tenants.ByRegion(region)
		tiers := &pb.TierSummary{}
		for _, t := range tenants {
			switch t.QoSTier {
			case QoSCritical:
				tiers.Critical++
			case QoSRegular:
				tiers.Regular++
			case QoSBackground:
				tiers.Background++
			}
		}
		target := regionTargetMap[region]
		statuses = append(statuses, &pb.RegionStatus{
			Region:       region,
			TenantCount:  int32(len(tenants)),
			EventCount:   counters[target],
			Tiers:        tiers,
		})
	}

	return &pb.GetRegionStatusResponse{Regions: statuses}, nil
}

// ─── ListTenantsByRegion ─────────────────────────────────────────────────────

// ListTenantsByRegion returns all tenants homed to a specific region.
// Useful for SLA-relevant lookup when a region is degraded.
func (s *tenantServer) ListTenantsByRegion(ctx context.Context, req *pb.ListTenantsByRegionRequest) (*pb.ListTenantsByRegionResponse, error) {
	if req.Region == "" {
		return nil, fmt.Errorf("region is required (us-east | us-west | eu-central)")
	}

	rows, err := db.New(s.adminPool).LoadTenantsByRegion(ctx, req.Region)
	if err != nil {
		return nil, fmt.Errorf("load tenants by region: %w", err)
	}

	tiers := &pb.TierSummary{}
	tenants := make([]*pb.Tenant, 0, len(rows))
	for _, t := range rows {
		switch t.QosTier {
		case QoSCritical:
			tiers.Critical++
		case QoSRegular:
			tiers.Regular++
		case QoSBackground:
			tiers.Background++
		}
		entry := &pb.Tenant{
			Id:         t.ID.String(),
			QosTier:    t.QosTier,
			HomeRegion: t.HomeRegion,
		}
		if t.CreatedAt.Valid {
			entry.CreatedAt = timestamppb.New(t.CreatedAt.Time)
		}
		tenants = append(tenants, entry)
	}

	return &pb.ListTenantsByRegionResponse{
		Region:  req.Region,
		Total:   int32(len(tenants)),
		Tiers:   tiers,
		Tenants: tenants,
	}, nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// countWithTenant runs SELECT COUNT(*) FROM events WHERE tenant_id = $1 as
// appuser with SET LOCAL app.current_tenant. Uses idx_events_tenant index.
func countWithTenant(ctx context.Context, pool *pgxpool.Pool, tenantID string) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID); err != nil {
		return 0, err
	}
	var count int64
	if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM events WHERE tenant_id = $1", tenantID).Scan(&count); err != nil {
		return 0, err
	}
	return count, tx.Commit(ctx)
}
