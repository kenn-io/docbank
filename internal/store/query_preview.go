package store

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/query"
)

// CompileQuery previews one query against a consistent definition snapshot.
// It does not execute a search or select a lexical generation. Snapshot
// execution uses the package compiler with its own transaction-bound resolver.
func (s *Store) CompileQuery(ctx context.Context, value query.Query) (compiled CompiledQuery, retErr error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return CompiledQuery{}, fmt.Errorf("acquiring query preview connection: %w", err)
	}
	active := false
	defer func() {
		if active {
			_, err := conn.ExecContext(context.Background(), "ROLLBACK")
			retErr = errors.Join(retErr, err)
		}
		retErr = errors.Join(retErr, conn.Close())
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return CompiledQuery{}, fmt.Errorf("starting query preview: %w", err)
	}
	active = true
	compiled, err = compileQuery(ctx, value, queryResolver{q: conn})
	if err != nil {
		return CompiledQuery{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return CompiledQuery{}, fmt.Errorf("committing query preview: %w", err)
	}
	active = false
	return compiled, nil
}
