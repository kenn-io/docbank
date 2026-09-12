package main

import (
	"context"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

type mailboxCancelReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *mailboxCancelReader) Read(p []byte) (int, error) {
	r.reads++
	r.cancel()
	return copy(p, "synthetic"), nil
}

func TestMailboxHashStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &mailboxCancelReader{cancel: cancel}
	_, err := hashMailboxSource(ctx, reader, 1<<30)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, reader.reads)
	_, err = hashMailboxSource(t.Context(), strings.NewReader("changed"), 3)
	require.Error(t, err)
}
