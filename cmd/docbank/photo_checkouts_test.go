package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoCheckoutCommandSelections(t *testing.T) {
	for _, tc := range []struct {
		args      []string
		years     *store.PhotoCheckoutYears
		all       bool
		wantError bool
	}{
		{args: []string{"estimate", "--years", "2024:2025"}, years: &store.PhotoCheckoutYears{From: 2024, To: 2025}},
		{args: []string{"estimate", "--years", "2024"}, years: &store.PhotoCheckoutYears{From: 2024, To: 2024}},
		{args: []string{"estimate", "--all"}, all: true},
		{args: []string{"estimate"}, wantError: true},
		{args: []string{"estimate", "--years", "2025:2024"}, wantError: true},
		{args: []string{"estimate", "--years", "bad"}, wantError: true},
		{args: []string{"estimate", "--asset", "bad"}, wantError: true},
	} {
		cmd := newPhotoCheckoutCmd()
		child, remaining, err := cmd.Find(tc.args)
		require.NoError(t, err)
		require.NoError(t, child.ParseFlags(remaining))
		selection, err := checkoutSelection(child)
		if tc.wantError {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.years, selection.Years)
			require.Equal(t, tc.all, selection.All)
		}
	}
}

func TestPhotoCheckoutCreateRejectsMissingCapacityBeforeDaemon(t *testing.T) {
	cmd := newPhotoCheckoutCmd()
	cmd.SetArgs([]string{"create", "destination", "--all"})
	require.ErrorContains(t, cmd.Execute(), "positive --byte-limit")
}
