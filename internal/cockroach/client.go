package cockroach

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog/log"

	"github.com/joshdurbin/cockroach_testing/internal/migrations"
)

// Pool wraps a pgxpool connection pool with helpers.
type Pool struct {
	*pgxpool.Pool
	sqlDB *sql.DB
}

// Connect opens a pgx pool and runs goose migrations. It retries the initial
// ping with exponential backoff for up to 60 seconds to tolerate transient
// "unexpected EOF" responses that occur when a CockroachDB node is still
// rejoining the cluster after a restart.
func Connect(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 50
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	// ConnectTimeout per individual connection attempt.
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	// Retry ping with backoff — nodes can return "unexpected EOF" while they
	// rejoin the cluster, and the pool should not give up immediately.
	if err := pingWithRetry(ctx, pool, 60*time.Second); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	p := &Pool{Pool: pool, sqlDB: sqlDB}

	if err := p.runMigrations(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrations: %w", err)
	}

	return p, nil
}

func pingWithRetry(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := time.Second
	var lastErr error
	for time.Now().Before(deadline) {
		if err := pool.Ping(ctx); err != nil {
			lastErr = err
			log.Debug().Err(err).Dur("retry_in", backoff).Msg("ping failed, retrying")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 8*time.Second {
				backoff *= 2
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("cluster not ready after %s: %w", timeout, lastErr)
}

func (p *Pool) runMigrations(ctx context.Context) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(gooseLogger{})

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	log.Info().Msg("running database migrations")
	if err := goose.Up(p.sqlDB, "."); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	log.Info().Msg("migrations complete")
	return nil
}

// ConnectRaw opens a pgx pool without running migrations.
// Use this for the appuser workload pool after migrations have already run
// via Connect() on the admin pool.
func ConnectRaw(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 50
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pingWithRetry(ctx, pool, 30*time.Second); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping appuser: %w", err)
	}
	return pool, nil
}

func (p *Pool) Close() {
	_ = p.sqlDB.Close()
	p.Pool.Close()
}

// gooseLogger bridges goose's logger to zerolog.
type gooseLogger struct{}

func (gooseLogger) Fatalf(format string, v ...any) {
	log.Fatal().Msgf(format, v...)
}
func (gooseLogger) Printf(format string, v ...any) {
	log.Debug().Msgf(format, v...)
}
