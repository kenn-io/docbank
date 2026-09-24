package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/production"
)

// PublishedProductionPackageHTTPFixture supplies the external Store test
// package with a real published job and three retained vault versions. The
// source, render, numbering, archive, and retention paths all run before the
// HTTP server is opened against the same vault root.
func PublishedProductionPackageHTTPFixture(t *testing.T) (*Store, string, production.Job,
	RetainedProductionPackage) {
	t.Helper()
	f, job := publishedRealRetentionFixture(t)
	dir := t.TempDir()
	request := production.RecipientPackageRequest{
		JobID: job.ID, ProfileID: "export-dat-opt-images-v1",
		Limits:          production.PackageLimits{MaxVolumeBytes: 50 << 20, MaxVolumeDocuments: 10},
		ArchivePath:     filepath.Join(dir, "recipient.zip"),
		QCPath:          filepath.Join(dir, "qc.json"),
		TransmittalPath: filepath.Join(dir, "transmittal.json"),
	}
	_, err := production.PublishRecipientPackage(t.Context(), f.Store, f, request)
	require.NoError(t, err)
	const operationID = "77777777-7777-4777-8777-777777777777"
	retained, err := f.RetainProductionPackage(t.Context(), job.ID, operationID,
		request.ProfileID, request.Limits, request.ArchivePath, request.QCPath,
		request.TransmittalPath, restartPackageBlobWriter(f))
	require.NoError(t, err)
	f.reopen(t)
	return f.Store, f.root, job, retained
}

// PublishedProductionJobHTTPFixture leaves a synthetic successful render
// without a recipient package so the public publication route can own it.
func PublishedProductionJobHTTPFixture(t *testing.T) (*Store, string, production.Job) {
	t.Helper()
	f, job := publishedRealRetentionFixture(t)
	f.reopen(t)
	return f.Store, f.root, job
}
