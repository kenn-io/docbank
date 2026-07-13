package api

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGateFreezerBlocksMutationOnlyUntilEnd(t *testing.T) {
	g := &gate{}
	freezer := &gateFreezer{gate: g}
	require.NoError(t, freezer.Begin(t.Context()))

	mutated := make(chan struct{})
	go func() {
		_ = g.mutate(func() error {
			close(mutated)
			return nil
		})
	}()
	select {
	case <-mutated:
		t.Fatal("mutation passed while backup freeze was held")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, freezer.End(context.Background()))
	select {
	case <-mutated:
	case <-time.After(time.Second):
		t.Fatal("mutation remained blocked after backup freeze ended")
	}
	require.Error(t, freezer.End(context.Background()))
}
