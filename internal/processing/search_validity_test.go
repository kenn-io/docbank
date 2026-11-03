package processing

import (
	"fmt"
	"math"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

func TestProcessingSearchValidityBeforeLimit(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	ctx := t.Context()
	old, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), "renewal-a", processingHash("old"), 1, "text/plain")
	require.NoError(t, err)
	next, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), "renewal-b", processingHash("new"), 1, "text/plain")
	require.NoError(t, err)
	request := SearchRequest{Query: "renewal", Mode: "lexical", Profile: "speech", Limit: 1, AsOf: "2025-06-01", Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{old.CurrentVersionID, next.CurrentVersionID}}}
	baseline, err := f.service.Search(ctx, request)
	require.NoError(t, err)
	require.Len(t, baseline.Results, 1)
	require.Equal(t, old.ID, baseline.Results[0].Document.NodeID, "precondition: old document wins before declarations")
	_, err = f.catalog.PutSupersession(ctx, next.ID, old.ID, old.Revision, new(""))
	require.NoError(t, err)
	_, err = f.catalog.PutEffectiveWindow(ctx, next.ID, next.Revision, "2025-01-01", "")
	require.NoError(t, err)
	report, err := f.service.Search(ctx, request)
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, next.ID, report.Results[0].Document.NodeID)
	require.Equal(t, 1, report.Results[0].Rank)
	require.True(t, report.Truncated)
	require.Equal(t, "2025-06-01", report.AsOf)
	request.AsOf = "2024-06-01"
	report, err = f.service.Search(ctx, request)
	require.NoError(t, err)
	require.Equal(t, old.ID, report.Results[0].Document.NodeID)
	require.Equal(t, document.ValidityCurrent, report.Results[0].Validity.State)
	request.Limit = 2
	request.AsOf = "2025-06-01"
	report, err = f.service.Search(ctx, request)
	require.NoError(t, err)
	require.Equal(t, document.ValiditySuperseded, report.Results[1].Validity.State)
	require.Equal(t, "/renewal-b", report.Results[1].Validity.SupersededBy[0].Path)
	require.Equal(t, math.Float64bits(baseline.Results[0].Score), math.Float64bits(report.Results[1].Score))
	request.Fence.ContentVersionIDs = []string{old.CurrentVersionID}
	report, err = f.service.Search(ctx, request)
	require.NoError(t, err)
	require.Nil(t, report.Results[0].Validity)
	request.AsOf = ""
	report, err = f.service.Search(ctx, request)
	require.NoError(t, err)
	require.Equal(t, time.Now().Format("2006-01-02"), report.AsOf)
	request.AsOf = "2025-02-30"
	require.ErrorIs(t, f.service.ValidateSearch(ctx, request), store.ErrInvalidSupersession)
	_, err = f.service.Search(ctx, request)
	require.ErrorIs(t, err, store.ErrInvalidSupersession)
}

func TestProcessingSearchValidityAllLiveScopeExcludesOutOfScopeSuccessor(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	ctx := t.Context()
	scope, err := f.catalog.Mkdir(ctx, f.catalog.RootID(), "in-scope")
	require.NoError(t, err)
	old, err := f.catalog.CreateFile(ctx, scope.ID, "leaseold", processingHash("scope-old"), 1, "text/plain")
	require.NoError(t, err)
	next, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), "leasenew", processingHash("scope-new"), 1, "text/plain")
	require.NoError(t, err)
	_, err = f.catalog.PutSupersession(ctx, next.ID, old.ID, old.Revision, new(""))
	require.NoError(t, err)
	report, err := f.service.Search(ctx, SearchRequest{Query: "leaseold", Mode: "lexical", Profile: "speech", AsOf: "2025-06-01", AllLive: &store.SearchOptions{UnderNodeID: scope.ID}})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, old.ID, report.Results[0].Document.NodeID)
	require.Nil(t, report.Results[0].Validity)
}

func TestMediaSearchValidityBeforeLimit(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	ctx := t.Context()
	var sources []retrieval.MediaSourceSelector
	var ids []string
	for i := range 2 {
		version := f.version
		if i == 1 {
			raw := mediatest.WAV()
			raw[len(raw)-1] = 1
			var selector Selector
			version, selector = f.addWAV(t, "replacement.wav", raw)
			plan, err := f.service.Plan(ctx, selector)
			require.NoError(t, err)
			_, err = f.service.GrantConsent(ctx, ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
			require.NoError(t, err)
		}
		retained, err := f.service.SubmitSuppliedMedia(ctx, SuppliedMediaRequest{OperationID: uuid.New().String(), Filename: "source.wav", MediaType: "audio/wav", SHA256: version.BlobHash, ByteLength: version.Size, ExistingContentVersionID: version.ID, Occurrence: MediaOccurrenceInput{Ref: fmt.Sprintf("message-%d", i), Revision: "1"}, Processing: &MediaProcessingRequest{Profile: "speech"}})
		require.NoError(t, err)
		require.NoError(t, f.run(t, retained.JobID))
		ids = append(ids, version.ID)
		sources = append(sources, retrieval.MediaSourceSelector{SourceID: retained.SourceID, SourceVersionID: retained.SourceVersionID, ContentVersionID: version.ID})
	}
	request := SearchRequest{Query: "Synthetic worker output", Mode: "lexical", Profile: "speech", Limit: 1, AsOf: "2025-01-01", Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: ids}, MediaSources: sources}
	baseline, err := f.service.Search(ctx, request)
	require.NoError(t, err)
	require.Len(t, baseline.Results, 1)
	old, err := f.catalog.NodeByID(ctx, baseline.Results[0].Document.NodeID)
	require.NoError(t, err)
	other := ids[0]
	if other == old.CurrentVersionID {
		other = ids[1]
	}
	next, err := f.catalog.ContentVersionByID(ctx, other)
	require.NoError(t, err)
	_, err = f.catalog.PutSupersession(ctx, next.NodeID, old.ID, old.Revision, new(""))
	require.NoError(t, err)
	report, err := f.service.Search(ctx, request)
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, next.NodeID, report.Results[0].Document.NodeID)
	require.Equal(t, 1, report.Results[0].Rank)
	require.True(t, report.Truncated)
	request.Limit = 2
	report, err = f.service.Search(ctx, request)
	require.NoError(t, err)
	require.Equal(t, document.ValiditySuperseded, report.Results[1].Validity.State)
	require.Equal(t, next.NodeID, report.Results[1].Validity.SupersededBy[0].NodeID)
}
