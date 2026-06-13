package cockroach

import (
	"context"

	crdbpgxv5 "github.com/cockroachdb/cockroach-go/v2/crdb/crdbpgxv5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExecTx runs fn inside a transaction, retrying on serialization failures using
// CockroachDB's SAVEPOINT cockroach_restart protocol via the official
// cockroach-go library. Any other error aborts immediately.
//
// The SAVEPOINT protocol is more efficient than rolling back and re-beginning
// because the server can restart the transaction without a new network round-trip.
func ExecTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return crdbpgxv5.ExecuteTx(ctx, pool, pgx.TxOptions{}, fn)
}
