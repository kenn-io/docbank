package processing

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"hegel.dev/go/hegel"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestPublishedNoteCoverageService(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	record := embeddingWorkerProfile(t, embeddingWorkerDescriptor(t))
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(record.CanonicalProfile, &profile))
	s := &Service{catalog: f.catalog, profiles: map[string]configuredProfile{"private": {portable: profile, record: record}}}
	scan, err := f.catalog.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{
		Name: "notebook", Destination: "/notes", PolicyFingerprint: workerHash("policy"), OperationID: uuid.NewString(),
	})
	require.NoError(t, err)
	receipt, err := f.catalog.AcceptPublishedNote(t.Context(), store.NotePublicationUploadRequest{
		Name: "notebook", ScanID: scan.ScanID, SourceRef: "note.md", FileName: "note.md", ParentPath: "/notes",
		OperationID: uuid.NewString(), Hash: workerHash("source"), Size: 4,
	}, store.BlobPhysical{Encoding: "raw", StoredBytes: 4, PackEligible: true, Created: true})
	require.NoError(t, err)
	want, err := s.Coverage(t.Context(), "private", SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{receipt.Source.Identity.ContentVersionID}})
	require.NoError(t, err)
	got, err := s.PublishedNoteCoverage(t.Context(), "private", store.PublishedNoteSelector{Publication: "notebook"})
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, 1, got.Renditions.Total)
	_, err = s.PublishedNoteCoverage(t.Context(), "missing", store.PublishedNoteSelector{})
	require.ErrorIs(t, err, ErrProfileNotConfigured)
	_, err = s.PublishedNoteCoverage(t.Context(), "private", store.PublishedNoteSelector{Publication: "INVALID"})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.PublishedNoteCoverage(ctx, "private", store.PublishedNoteSelector{})
	require.ErrorIs(t, err, context.Canceled)
	empty, err := s.PublishedNoteCoverage(t.Context(), "private", store.PublishedNoteSelector{Publication: "missing"})
	require.NoError(t, err)
	require.Zero(t, empty.Renditions.Total)
	for _, binding := range empty.Embeddings {
		require.Zero(t, binding.Total)
	}
}

// Each generated cell has an independent status. The worst applicable status
// wins: missing outranks rebuilding, required embeddings suppress optional ones.
// Counts and identity must survive the shared report adapter unchanged.
func TestPublishedNoteCoverageReportProperty(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		t.Skip("Hegel native engine requires macOS arm64")
	}
	f := newPublicationFixture(t)
	s := &Service{catalog: f.catalog}
	hegel.Test(t, func(ht *hegel.T) {
		profile := configuredProfile{record: store.ProcessingProfileRecord{Fingerprint: workerHash("report-profile")}}
		renditionMode := hegel.Draw(ht, hegel.Integers(0, 2))
		if renditionMode > 0 {
			profile.portable.Rendition = &document.RenditionBindingV1{}
			if renditionMode == 2 {
				profile.portable.Rendition.AdapterContract = document.VisionAdapterContract
			}
		}
		total := hegel.Draw(ht, hegel.Integers(0, 8))
		snapshot := store.ProcessingCoverageSnapshot{}
		var severity []int
		drawClass := func(name string, required bool) store.ProcessingClassCoverage {
			item := store.ProcessingClassCoverage{Name: name, Required: required, State: "fixture-state", Total: total}
			worst := 0
			for range total {
				state := hegel.Draw(ht, hegel.Integers(0, 4))
				switch state {
				case 0:
					item.Complete++
				case 1:
					item.Rebuilding++
					if hegel.Draw(ht, hegel.Booleans()) {
						item.PreviousGenerationServing++
					}
					worst = max(worst, 1)
				case 2:
					item.Unavailable++
					worst = 2
				case 3:
					item.Stale++
					worst = 2
				case 4:
					item.Ineligible++
					worst = 2
				}
			}
			severity = append(severity, worst)
			return item
		}
		snapshot.Renditions = drawClass("rendition", false)
		bindingCount := hegel.Draw(ht, hegel.Integers(0, 6))
		anyRequired := false
		for i := range bindingCount {
			required := hegel.Draw(ht, hegel.Booleans())
			anyRequired = anyRequired || required
			snapshot.Embeddings = append(snapshot.Embeddings, drawClass(fmt.Sprintf("binding-%d", i), required))
		}
		worst := 0
		if renditionMode == 1 {
			worst = severity[0]
		}
		for i, item := range snapshot.Embeddings {
			if item.Required || !anyRequired {
				worst = max(worst, severity[i+1])
			}
		}
		wantState := []string{"complete", "rebuilding", "partial"}[worst]
		got := s.coverageReport(profile, snapshot)
		require.Equal(ht, wantState, got.State)
		require.Equal(ht, f.catalog.VaultID(), got.VaultUID)
		require.Equal(ht, profile.record.Fingerprint, got.ProfileFingerprint)
		wantR := CoverageClass{Name: "rendition", State: "not_required", Total: total}
		if renditionMode == 1 {
			item := snapshot.Renditions
			wantR = CoverageClass{Name: "rendition", Required: true, State: "fixture-state", Total: total,
				Complete: item.Complete, Rebuilding: item.Rebuilding, PreviousServing: item.PreviousGenerationServing,
				Unavailable: item.Unavailable, Stale: item.Stale, Ineligible: item.Ineligible}
		}
		require.Equal(ht, wantR, got.Renditions)
		require.Len(ht, got.Embeddings, len(snapshot.Embeddings))
		for i, item := range snapshot.Embeddings {
			require.Equal(ht, CoverageClass{Name: item.Name, Required: item.Required, State: item.State, Total: total,
				Complete: item.Complete, Rebuilding: item.Rebuilding, PreviousServing: item.PreviousGenerationServing,
				Unavailable: item.Unavailable, Stale: item.Stale, Ineligible: item.Ineligible}, got.Embeddings[i])
		}
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(3000))
}
