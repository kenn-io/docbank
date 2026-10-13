package main

import (
	"encoding/json/v2"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"image/color"
	"testing"
	"time"

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

func TestPhotoCheckoutCreateRejectsNegativeLimitBeforeDaemon(t *testing.T) {
	for _, selector := range []string{"--asset", "--album"} {
		cmd := newPhotoCheckoutCmd()
		cmd.SetArgs([]string{"create", "destination", selector, "00000000-0000-4000-8000-000000000001", "--byte-limit", "-1"})
		require.ErrorContains(t, cmd.Execute(), "--byte-limit must be nonnegative")
	}
}

func TestPhotoCheckoutCLICreateListStatus(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "checkout-image.jpeg", string(mediatest.JPEG(2, 2, color.White)))
	_, err := runCLI(t, "add", source, "--dest", "/inbox")
	require.NoError(t, err)
	out, err := runCLI(t, "photos", "checkout", "create", t.TempDir(), "--all", "--byte-limit", "100000")
	require.NoError(t, err, out)
	var created api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	out, err = runCLI(t, "photos", "checkout", "list")
	require.NoError(t, err, out)
	var list []store.PhotoCheckout
	require.NoError(t, json.Unmarshal([]byte(out), &list))
	require.Len(t, list, 1)
	require.Equal(t, created.Checkout.ID, list[0].ID)
	require.Eventually(t, func() bool {
		out, err = runCLI(t, "photos", "checkout", "status", created.Checkout.ID)
		if err != nil {
			return false
		}
		var status api.PhotoCheckoutStatus
		require.NoError(t, json.Unmarshal([]byte(out), &status))
		require.NotContains(t, out, `"entries"`)
		return status.Checkout.State == "completed" && status.Files == 1 && status.PublishedFiles == 1 && status.Bytes == status.PublishedBytes
	}, 5*time.Second, 20*time.Millisecond)
}
