package store

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkRenditionConnectionFailurePostgres(t *testing.T) {
	t.Helper()
	for _, failure := range []string{"before write", "commit response"} {
		t.Run(failure, func(t *testing.T) {
			setup, owner := pgtest.NewDatabase(t)
			s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
			ctx := t.Context()
			versions := seedRenditionCatalogVersions(t, s)
			request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
			grantRenditionJobConsent(t, s, request)
			job, _, err := s.EnqueueRenditionJob(ctx, request)
			require.NoError(t, err)
			conn, err := s.db.Conn(ctx)
			require.NoError(t, err)
			var config *pgx.ConnConfig
			err = conn.Raw(func(raw any) error {
				pg, ok := raw.(*stdlib.Conn)
				if !ok {
					return fmt.Errorf("unexpected PostgreSQL connection %T", raw)
				}
				config = pg.Conn().Config().Copy()
				return nil
			})
			require.NoError(t, conn.Close())
			require.NoError(t, err)
			var dropResponse atomic.Bool
			var injected bool
			var injectionErr error
			config.Tracer = renditionQueryStart(func(ctx context.Context, conn *pgx.Conn, query pgx.TraceQueryStartData) {
				if injected {
					return
				}
				if failure == "before write" && strings.Contains(query.SQL, "SET state='running',claim_owner=") {
					injectionErr = setup.QueryRowContext(ctx, `SELECT pg_terminate_backend($1,5000)`, conn.PgConn().PID()).Scan(&injected)
				} else if failure == "commit response" && query.SQL == "commit" {
					injected = true
					dropResponse.Store(true)
				}
			})
			dial := config.DialFunc
			config.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return &renditionLostResponseConn{Conn: conn, drop: &dropResponse}, nil
			}
			writer := stdlib.OpenDB(*config)
			t.Cleanup(func() { require.NoError(t, writer.Close()) })
			s.writeDB = writer
			at := time.Now().UTC().Add(time.Second)
			_, _, err = s.ClaimNextRenditionJob(ctx, "first-worker", at, time.Minute)
			require.NoError(t, injectionErr)
			require.True(t, injected)
			require.Error(t, err)
			if failure == "before write" {
				require.True(t, s.RenditionJobErrorRetryable(err), "a disconnected, uncommitted claim must not stop the worker: %v", err)
				stored, err := s.RenditionJobByID(ctx, job.ID)
				require.NoError(t, err)
				require.Equal(t, RenditionJobQueued, stored.State)
				claim, found, err := s.ClaimNextRenditionJob(ctx, "first-worker", at, time.Minute)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, job.ID, claim.JobID)
				require.EqualValues(t, 1, claim.Epoch)
			} else {
				require.False(t, s.RenditionJobErrorRetryable(err), "the worker cannot retry an unknown commit outcome: %v", err)
				stored, err := s.RenditionJobByID(ctx, job.ID)
				require.NoError(t, err)
				require.Equal(t, RenditionJobRunning, stored.State, "the claim committed despite the lost response")
				require.EqualValues(t, 1, stored.ClaimEpoch)
			}
		})
	}
}

type renditionQueryStart func(context.Context, *pgx.Conn, pgx.TraceQueryStartData)

func (trace renditionQueryStart) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	trace(ctx, conn, data)
	return ctx
}

func (renditionQueryStart) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type renditionLostResponseConn struct {
	net.Conn

	drop *atomic.Bool
}

func (conn *renditionLostResponseConn) Read(p []byte) (int, error) {
	n, err := conn.Conn.Read(p)
	if n > 0 && conn.drop.Swap(false) {
		return 0, io.ErrUnexpectedEOF
	}
	return n, err //nolint:wrapcheck // Preserve the underlying net.Conn error outside the injected response loss.
}
