package main

import (
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/cenkalti/backoff/v7"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
)

func TestReportFamilyExplicitOriginalExports(t *testing.T) {
	f := newFamilyReportFixture(t, "selected")
	stop := f.start(t, f.root)
	client := startFamilyMCP(t)
	var saved []struct {
		path      string
		receipt   bundle.Receipt
		originals []reportOriginal
	}
	for _, keys := range [][]string{{"P", "C"}, {"P", "C", "U"}} {
		var originals []reportOriginal
		for _, key := range keys {
			originals = append(originals, f.originals[key])
		}
		cliPath, cliReceipt := exportReportOriginals(t, originals)
		mcpPath, mcpReceipt := client.exportOriginals(t, originals)
		for path, receipt := range map[string]bundle.Receipt{
			cliPath: cliReceipt, mcpPath: mcpReceipt,
		} {
			assertOriginalArchive(t, path, receipt, originals)
			saved = append(saved, struct {
				path      string
				receipt   bundle.Receipt
				originals []reportOriginal
			}{path, receipt, originals})
		}
	}
	client.stop(t)
	stop()
	for _, archive := range saved {
		assertOriginalArchive(t, archive.path, archive.receipt, archive.originals)
	}
}

func (c *familyMCP) exportOriginals(
	t *testing.T, originals []reportOriginal,
) (string, bundle.Receipt) {
	t.Helper()
	request := exportPreviewRequest{
		SourceOperationID: uuid.New().String(), PlanOperationID: uuid.New().String(),
	}
	for _, original := range originals {
		request.Members = append(request.Members, original.member)
	}
	raw := c.call(t, "preview_export", request)
	var preview struct {
		Plan bundle.Plan `json:"plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &preview))
	plan := preview.Plan
	require.NotEmpty(t, plan.ID, string(raw))
	require.Equal(t, len(originals), plan.Total)
	require.Equal(t, []bundle.RolePolicy{{Role: "original"}}, plan.Roles)
	jobID := uuid.New().String()
	raw = c.call(t, "start_export", bundle.JobRequest{
		OperationID: jobID, PlanID: plan.ID, Fingerprint: plan.Fingerprint,
	})
	var started struct {
		Job bundle.Job `json:"job"`
	}
	require.NoError(t, json.Unmarshal(raw, &started))
	require.Equal(t, jobID, started.Job.ID, string(raw))
	args := map[string]any{"job_id": jobID}
	job, err := backoff.Retry(t.Context(), func() (bundle.Job, error) {
		var status struct {
			Job bundle.Job `json:"job"`
		}
		raw := c.call(t, "get_export_status", args)
		require.NoError(t, json.Unmarshal(raw, &status))
		require.Equal(t, jobID, status.Job.ID, string(raw))
		if status.Job.State == "queued" || status.Job.State == "running" {
			return status.Job, errors.New("export still active")
		}
		return status.Job, nil
	}, backoff.WithBackOff(backoff.NewConstantBackOff(25*time.Millisecond)),
		backoff.WithMaxElapsedTime(30*time.Second))
	require.NoError(t, err)
	require.Equal(t, "completed", job.State, job.Failure)
	path := filepath.Join(t.TempDir(), "originals.zip")
	raw = c.call(t, "download_export", map[string]any{
		"job_id": jobID, "destination_path": path,
	})
	var download struct {
		State   string         `json:"state"`
		Receipt bundle.Receipt `json:"receipt"`
	}
	require.NoError(t, json.Unmarshal(raw, &download))
	require.Equal(t, "published", download.State, string(raw))
	require.Equal(t, plan.Fingerprint, download.Receipt.PlanFingerprint)
	require.NotNil(t, job.Receipt)
	require.Equal(t, *job.Receipt, download.Receipt)
	_, err = backoff.Retry(t.Context(), func() (bool, error) {
		var release struct {
			Code     string `json:"code"`
			Released bool   `json:"released"`
		}
		raw := c.call(t, "release_export", args)
		require.NoError(t, json.Unmarshal(raw, &release))
		if release.Code == "export_retained" {
			return false, errors.New("download lease still retained")
		}
		require.True(t, release.Released, string(raw))
		return true, nil
	}, backoff.WithBackOff(backoff.NewConstantBackOff(25*time.Millisecond)),
		backoff.WithMaxElapsedTime(30*time.Second))
	require.NoError(t, err)
	raw = c.call(t, "get_export_status", args)
	var missing struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(raw, &missing))
	require.Equal(t, "not_found", missing.Code, string(raw))
	return path, download.Receipt
}
