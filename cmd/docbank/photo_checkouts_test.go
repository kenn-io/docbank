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
		{args: []string{"estimate", "--years", "bad"}, wantError: true},
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

func TestPhotoCheckoutCreateRejectsInvalidCapacityBeforeDaemon(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"create", "destination", "--all"}, "positive --byte-limit"},
		{[]string{"create", "destination", "--asset", "00000000-0000-4000-8000-000000000001", "--byte-limit", "-1"}, "--byte-limit must be nonnegative"},
	} {
		cmd := newPhotoCheckoutCmd()
		cmd.SetArgs(tc.args)
		require.ErrorContains(t, cmd.Execute(), tc.want)
	}
}

func TestPhotoCheckoutCLICreateListStatus(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "checkout-image.jpeg", string(mediatest.JPEG(2, 2, color.White)))
	_, err := runCLI(t, "add", source, "--dest", "/inbox")
	require.NoError(t, err)
	destination := t.TempDir()
	out, err := runCLI(t, "photos", "checkout", "create", destination, "--all", "--byte-limit", "100000")
	require.NoError(t, err, out)
	var created api.PhotoCheckoutCreateResult
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	require.True(t, created.Checkout.Selection.All)
	require.Equal(t, int64(100000), created.Checkout.ByteLimit)
	require.Equal(t, destination, created.Checkout.Destination)
	out, err = runCLI(t, "photos", "checkout", "list")
	require.NoError(t, err, out)
	var list []store.PhotoCheckout
	require.NoError(t, json.Unmarshal([]byte(out), &list))
	require.Equal(t, created.Checkout.ID, list[0].ID)
	out, err = runCLI(t, "photos", "checkout", "status", created.Checkout.ID)
	require.NoError(t, err, out)
	var status api.PhotoCheckoutStatus
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	require.Equal(t, created.Checkout.ID, status.Checkout.ID)
	require.Eventually(t, func() bool {
		out, err := runCLI(t, "jobs", "show", created.Operation.ID, "--json")
		if err != nil {
			return false
		}
		var operation api.StorageOperation
		if json.Unmarshal([]byte(out), &operation) != nil {
			return false
		}
		return operation.FinishedAt != ""
	}, 5*time.Second, 20*time.Millisecond)

}
