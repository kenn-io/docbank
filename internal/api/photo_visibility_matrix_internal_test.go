package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoOwnerMaintenanceGate(t *testing.T) {
	g := NewOperationGate()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Error(t, g.MutateContext(ctx, func() error { return nil }))
	require.NoError(t, g.mutate(func() error { return nil }))
}
