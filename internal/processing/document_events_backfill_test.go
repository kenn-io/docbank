package processing

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestDocumentEventRestoreDrainPublishesAnEmptyVault(t *testing.T) {
	t.Parallel()
	catalog, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })

	require.NoError(t, RebuildDocumentEvents(t.Context(), catalog))
	targets, err := catalog.MissingDocumentEventTargetsAfter(
		t.Context(), DocumentEventsDeriverFingerprint, "", 25,
	)
	require.NoError(t, err)
	require.Empty(t, targets)
}

func TestDocumentEventEmailActorsMatchPersonIdentity(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"ÜSER@EXAMPLE.TEST", "USER@İ.example.test", `"Ada Lovelace"@example.test`, `"<Ada"@example.test`, `" Ada"@example.test`} {
		t.Run(address, func(t *testing.T) {
			catalog := openDocumentEventTestStore(t)
			ctx := t.Context()
			created, err := catalog.CreateFile(ctx, catalog.RootID(), "mail.eml", testDigest("unicode-email"), 8, "message/rfc822")
			require.NoError(t, err)
			publishDocumentEventTestMetadata(t, catalog, created.BlobHash, "email-metadata", []document.SourceMetadataFieldV1{
				stringField("email.from", "From", false, address),
			})
			require.NoError(t, RebuildDocumentEvents(ctx, catalog))
			view, err := catalog.DocumentEventsForVersion(ctx, created.CurrentVersionID)
			require.NoError(t, err)
			event := requireEventKind(t, view.Events.Events, "vault_recorded")
			require.Len(t, event.Actors, 1)

			person, err := catalog.CreatePerson(ctx, "Synthetic sender", "operator")
			require.NoError(t, err)
			_, err = catalog.AddPersonIdentity(ctx, person.PersonID, person.Revision, store.PersonIdentity{
				Kind: "email", ValueDisplay: address, Origin: "operator", EvidenceKind: "operator_assertion",
				EvidenceID: "sender-claim", Confidence: "operator_asserted",
			})
			require.NoError(t, err)
			keys, err := catalog.ActorKeysForPerson(ctx, store.PersonActorKeysRequest{PersonID: person.PersonID})
			require.NoError(t, err)
			require.Equal(t, []string{event.Actors[0].ActorKey}, keys.Items)
		})
	}
}

func TestDocumentEventRestoreDrainIndexesEveryRetainedVersion(t *testing.T) {
	t.Parallel()
	catalog := openDocumentEventTestStore(t)
	created, err := catalog.CreateFile(
		t.Context(), catalog.RootID(), "history.pdf", testDigest("history-a"), 8, "application/pdf",
	)
	require.NoError(t, err)
	historicalVersionID := created.CurrentVersionID
	publishDocumentEventTestMetadata(t, catalog, created.BlobHash, "metadata-a", nil)

	updated, current, err := catalog.ReplaceContent(
		t.Context(), created.ID, created.Revision, testDigest("history-b"), 9, "application/pdf",
	)
	require.NoError(t, err)
	require.Equal(t, current.ID, updated.CurrentVersionID)
	publishDocumentEventTestMetadata(t, catalog, current.BlobHash, "metadata-b", nil)

	require.NoError(t, RebuildDocumentEvents(t.Context(), catalog))
	coverage, err := catalog.DocumentEventRebuildCoverage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(2), coverage.Selected)
	require.Equal(t, int64(2), coverage.Indexed)
	require.Zero(t, coverage.Pending)

	_, err = catalog.DocumentEventsForVersion(t.Context(), historicalVersionID)
	require.NoError(t, err)
	_, err = catalog.DocumentEventsForVersion(t.Context(), current.ID)
	require.NoError(t, err)
}

func TestDocumentEventRebuildDoesNotRecreatePurgedRendition(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	_, err = publisher.PublishRendition(t.Context(), fixture.stage(
		t, publicationIDs{"d71", "d72", "d73"},
		"synthetic timeline evidence", "synthetic timeline markdown",
	))
	require.NoError(t, err)
	publishDocumentEventTestMetadata(
		t, fixture.catalog, fixture.mustSourceHash(), "timeline-metadata",
		[]document.SourceMetadataFieldV1{{
			Key: "created", Namespace: "pdf.info", SourceField: "CreationDate",
			Value: document.SourceMetadataValueV1{
				Kind: document.SourceMetadataTimestamp,
				Timestamp: &document.SourceMetadataTimestampV1{
					Normalized: "2024-01-02", Raw: "D:20240102",
					Precision: document.SourceMetadataPrecisionDate,
					Timezone:  document.SourceMetadataTimezoneOmitted,
				},
			},
		}},
	)
	metadata, _, err := fixture.catalog.ActiveSourceMetadata(
		t.Context(), fixture.mustSourceHash(),
	)
	require.NoError(t, err)
	assertSourceDate := func() {
		view, viewErr := fixture.catalog.DocumentEventsForVersion(t.Context(), fixture.versionID)
		require.NoError(t, viewErr)
		for _, event := range view.Events.Events {
			if event.SourceKey == "metadata/"+metadata.GenerationID+"/created/0" {
				require.Equal(t, document.DateKind("created"), event.DateKind)
				require.Equal(t, "2024-01-02", event.DateValue)
				return
			}
		}
		require.Fail(t, "missing original-source date event")
	}
	require.NoError(t, RebuildDocumentEvents(t.Context(), fixture.catalog))
	assertSourceDate()

	purge, err := fixture.catalog.PurgeDerivatives(
		t.Context(), store.PurgeRequest{ContentVersionIDs: []string{fixture.versionID}},
	)
	require.NoError(t, err)
	require.Equal(t, 1, purge.RemovedHeads)
	_, err = fixture.catalog.ActiveRendition(
		t.Context(), fixture.versionID, fixture.profile.Fingerprint,
	)
	require.ErrorIs(t, err, store.ErrNotFound)
	assertSourceDate()

	require.NoError(t, RebuildDocumentEvents(t.Context(), fixture.catalog))
	assertSourceDate()
	_, err = fixture.catalog.ActiveRendition(
		t.Context(), fixture.versionID, fixture.profile.Fingerprint,
	)
	require.ErrorIs(t, err, store.ErrNotFound,
		"the timeline worker must not recreate a purged rendition")
}

func TestDocumentEventRestoreDrainRecordsUnavailableUntilANewEpoch(t *testing.T) {
	t.Parallel()
	catalog := openDocumentEventTestStore(t)
	created, err := catalog.CreateFile(
		t.Context(), catalog.RootID(), "over-limit.eml", testDigest("over-limit"), 10, "message/rfc822",
	)
	require.NoError(t, err)
	claim := strings.Repeat("x", document.MaxDocumentEventActorClaimBytes+1)
	publishDocumentEventTestMetadata(t, catalog, created.BlobHash, "over-limit-metadata",
		[]document.SourceMetadataFieldV1{{
			Key: "email.to", Namespace: "email", SourceField: "To",
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: &claim},
		}},
	)

	require.NoError(t, RebuildDocumentEvents(t.Context(), catalog))
	coverage, err := catalog.DocumentEventRebuildCoverage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), coverage.Selected)
	require.Equal(t, int64(1), coverage.Unavailable)
	require.Zero(t, coverage.Indexed)
	require.Zero(t, coverage.Pending)

	_, err = catalog.DocumentEventsForVersion(t.Context(), created.CurrentVersionID)
	require.ErrorIs(t, err, store.ErrNotFound)
	targets, err := catalog.MissingDocumentEventTargetsAfter(
		t.Context(), DocumentEventsDeriverFingerprint, "", 25,
	)
	require.NoError(t, err)
	require.Empty(t, targets, "the matching terminal attempt suppresses a hot retry")

	_, err = catalog.BumpDocumentEventInputEpoch(t.Context(), DocumentEventsDeriverFingerprint)
	require.NoError(t, err)
	targets, err = catalog.MissingDocumentEventTargetsAfter(
		t.Context(), DocumentEventsDeriverFingerprint, "", 25,
	)
	require.NoError(t, err)
	require.Len(t, targets, 1, "a new epoch makes the terminal target eligible again")
}

func TestDocumentEventRestoreDrainPreservesOutOfRangeProvenanceTime(t *testing.T) {
	t.Parallel()
	catalog := openDocumentEventTestStore(t)
	run, err := catalog.BeginIngest(t.Context(), "cli", "/synthetic")
	require.NoError(t, err)
	const mtime = "0000-01-02T03:04:05Z"
	node, err := catalog.IngestFileExact(t.Context(), run, catalog.RootID(), "old.txt",
		testDigest("old"), 1, "text/plain", "/synthetic/old.txt", mtime)
	require.NoError(t, err)

	require.NoError(t, RebuildDocumentEvents(t.Context(), catalog))
	coverage, err := catalog.DocumentEventRebuildCoverage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), coverage.Unavailable)
	require.Zero(t, coverage.Pending+coverage.Failed)
	provenance, err := catalog.NodeProvenance(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, provenance.Items, 1)
	require.Equal(t, new(mtime), provenance.Items[0].OriginalMTime)
}

func TestDocumentEventRestoreDrainRecordsAggregateOutputAsUnavailable(t *testing.T) {
	t.Parallel()
	catalog := openDocumentEventTestStore(t)
	created, err := catalog.CreateFile(
		t.Context(), catalog.RootID(), "aggregate.eml", testDigest("aggregate"), 10, "message/rfc822",
	)
	require.NoError(t, err)
	header := strings.TrimSuffix(strings.Repeat("a@b,", 900), ",")
	publishDocumentEventTestMetadata(t, catalog, created.BlobHash, "aggregate-metadata",
		[]document.SourceMetadataFieldV1{
			stringField("email.to", "To", false, header),
			stringField("email.cc", "Cc", false, header),
			stringField("email.bcc", "Bcc", false, header),
		},
	)

	require.NoError(t, RebuildDocumentEvents(t.Context(), catalog))
	coverage, err := catalog.DocumentEventRebuildCoverage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), coverage.Selected)
	require.Equal(t, int64(1), coverage.Unavailable)
	require.Zero(t, coverage.Indexed)
	require.Zero(t, coverage.Pending)
	require.Zero(t, coverage.Failed)
	_, err = catalog.DocumentEventsForVersion(t.Context(), created.CurrentVersionID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestDocumentEventRestoreCompletionRejectsPendingAndFailed(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name     string
		coverage store.DocumentEventCoverage
		want     bool
	}{
		{name: "pending", coverage: store.DocumentEventCoverage{Pending: 1}, want: true},
		{name: "failed", coverage: store.DocumentEventCoverage{Failed: 1}, want: true},
		{name: "unavailable", coverage: store.DocumentEventCoverage{Unavailable: 1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(t, testCase.want, documentEventRestoreIncomplete(testCase.coverage))
		})
	}
}

func TestDocumentEventBackfillRestartDoesNotCompleteAStaleBuild(t *testing.T) {
	t.Parallel()
	catalog := openDocumentEventTestStore(t)
	created, err := catalog.CreateFile(
		t.Context(), catalog.RootID(), "stale.pdf", testDigest("stale"), 8, "application/pdf",
	)
	require.NoError(t, err)
	publishDocumentEventTestMetadata(t, catalog, created.BlobHash, "metadata-before", nil)
	build, err := catalog.StartDocumentEventRebuild(
		t.Context(), "90000000-0000-4000-8000-000000000009", testDigest("rebuild-request"),
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	staleCatalog := &documentEventStaleOnceCatalog{
		Store: catalog,
		mutate: func() {
			publishDocumentEventTestMetadata(t, catalog, created.BlobHash, "metadata-after",
				[]document.SourceMetadataFieldV1{{
					Key: "title", Namespace: "pdf.info", SourceField: "Title",
					Value: document.SourceMetadataValueV1{
						Kind: document.SourceMetadataString, String: new("changed"),
					},
				}},
			)
		},
		cancel: cancel,
	}
	first := NewDocumentEventBackfill(staleCatalog, nil, nil)
	first.DrainOnce = true
	err = first.Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, staleCatalog.publishErr, store.ErrDocumentEventInputsChanged)

	require.NoError(t, catalog.RefreshDocumentEventBuilds(t.Context()))
	build, err = catalog.DocumentEventBuild(t.Context(), build.OperationID)
	require.NoError(t, err)
	require.Equal(t, "running", build.State)
	require.Zero(t, build.Scanned)
	targets, err := catalog.MissingDocumentEventTargetsAfter(
		t.Context(), DocumentEventsDeriverFingerprint, "", 25,
	)
	require.NoError(t, err)
	require.Len(t, targets, 1)

	restarted := NewDocumentEventBackfill(catalog, nil, nil)
	restarted.DrainOnce = true
	require.NoError(t, restarted.Run(t.Context()))
	build, err = catalog.DocumentEventBuild(t.Context(), build.OperationID)
	require.NoError(t, err)
	require.Equal(t, "completed", build.State)
	require.Equal(t, int64(1), build.Scanned)
	require.Equal(t, int64(1), build.Published)
}

func TestDocumentEventBackfillReportsProgressBeforeTheScanDrains(t *testing.T) {
	t.Parallel()
	catalog := openDocumentEventTestStore(t)
	for _, name := range []string{"first.txt", "second.txt"} {
		_, err := catalog.CreateFile(t.Context(), catalog.RootID(), name, testDigest(name), 1, "text/plain")
		require.NoError(t, err)
	}
	build, err := catalog.StartDocumentEventRebuild(t.Context(),
		"93000000-0000-4000-8000-000000000009", testDigest("progress"))
	require.NoError(t, err)
	backfill := NewDocumentEventBackfill(catalog, nil, nil)
	now := time.Date(2026, time.September, 12, 1, 2, 3, 0, time.UTC)
	backfill.Now = func() time.Time { return now }
	first, err := backfill.List(t.Context(), "", 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.NoError(t, backfill.Process(t.Context(), first[0]))
	now = now.Add(500 * time.Millisecond)
	next, err := backfill.List(t.Context(), backfill.Key(first[0]), 1)
	require.NoError(t, err)
	require.Len(t, next, 1, "the scan still has another page")
	build, err = catalog.DocumentEventBuild(t.Context(), build.OperationID)
	require.NoError(t, err)
	require.Zero(t, build.Scanned, "progress refreshes are limited to once per second")
	now = now.Add(500 * time.Millisecond)
	next, err = backfill.List(t.Context(), backfill.Key(first[0]), 1)
	require.NoError(t, err)
	require.Len(t, next, 1)

	build, err = catalog.DocumentEventBuild(t.Context(), build.OperationID)
	require.NoError(t, err)
	require.Equal(t, "running", build.State)
	require.Equal(t, int64(1), build.Scanned)
	require.Equal(t, int64(1), build.Published)
}

func TestDocumentEventBackfillGatesEachMutationOnce(t *testing.T) {
	t.Parallel()
	target := store.DocumentEventTarget{
		ContentVersionID: "91000000-0000-4000-8000-000000000009",
		NodeID:           1, BlobHash: testDigest("gated"), Size: 1, MIMEType: "text/plain",
		RecordedAt: "2026-09-12T01:02:03Z", InputEpoch: 1,
	}
	catalog := &gatedDocumentEventCatalog{
		target: target,
		snapshot: store.DocumentEventEvidenceSnapshot{
			VaultUID: "91000000-0000-4000-8000-000000000010",
			Target:   target, Bindings: []store.BoundProvenanceEventInput{},
			InputsSHA256: testDigest("gated-inputs"),
		},
		pending: true,
	}
	mutate := func(_ context.Context, fn func() error) error {
		if catalog.gateDepth != 0 {
			return errors.New("document event mutation gate was nested")
		}
		catalog.gateDepth++
		catalog.gateCalls++
		defer func() { catalog.gateDepth-- }()
		return fn()
	}
	backfill := NewDocumentEventBackfill(catalog, mutate, nil)
	backfill.DrainOnce = true

	require.NoError(t, backfill.Run(t.Context()))
	require.Equal(t, 5, catalog.gateCalls)
	require.Equal(t, 2, catalog.ensureCalls)
	require.Equal(t, 1, catalog.publishCalls)
	require.Equal(t, 2, catalog.refreshCalls)
}

func TestDocumentEventBackfillClassifiesOnlyCapturedTerminalEvidence(t *testing.T) {
	t.Parallel()
	target := store.DocumentEventTarget{ContentVersionID: "92000000-0000-4000-8000-000000000009"}
	t.Run("corruption wins over exhaustion", func(t *testing.T) {
		catalog := &documentEventFailureCatalog{
			snapshot: store.DocumentEventEvidenceSnapshot{InputsSHA256: testDigest("captured")},
			loadErr: errors.Join(
				store.ErrDocumentEventEvidenceUnavailable, store.ErrSourceMetadataCorrupt,
			),
		}
		completed, err := BackfillDocumentEventTargets(
			t.Context(), catalog, []store.DocumentEventTarget{target},
		)
		require.NoError(t, err)
		require.Equal(t, 1, completed)
		require.Equal(t, "failed", catalog.recordedState)
		require.Equal(t, 1, catalog.attempts)
	})
	t.Run("missing digest remains transient", func(t *testing.T) {
		catalog := &documentEventFailureCatalog{loadErr: store.ErrSourceMetadataCorrupt}
		completed, err := BackfillDocumentEventTargets(
			t.Context(), catalog, []store.DocumentEventTarget{target},
		)
		require.ErrorIs(t, err, store.ErrSourceMetadataCorrupt)
		require.Zero(t, completed)
		require.Zero(t, catalog.attempts)
	})
}

type gatedDocumentEventCatalog struct {
	target       store.DocumentEventTarget
	snapshot     store.DocumentEventEvidenceSnapshot
	pending      bool
	gateDepth    int
	gateCalls    int
	ensureCalls  int
	publishCalls int
	refreshCalls int
}

func (catalog *gatedDocumentEventCatalog) requireGate() error {
	if catalog.gateDepth != 1 {
		return errors.New("document event mutation ran outside its gate")
	}
	return nil
}

func (catalog *gatedDocumentEventCatalog) EnsureDocumentEventRecipe(context.Context, string) error {
	if err := catalog.requireGate(); err != nil {
		return err
	}
	catalog.ensureCalls++
	return nil
}

func (catalog *gatedDocumentEventCatalog) MissingDocumentEventTargetsAfter(
	_ context.Context, _ string, after string, _ int,
) ([]store.DocumentEventTarget, error) {
	if catalog.gateDepth != 0 {
		return nil, errors.New("document event scan ran inside its mutation gate")
	}
	if catalog.pending && after == "" {
		return []store.DocumentEventTarget{catalog.target}, nil
	}
	return []store.DocumentEventTarget{}, nil
}

func (catalog *gatedDocumentEventCatalog) LoadDocumentEventEvidence(
	context.Context, store.DocumentEventTarget,
) (store.DocumentEventEvidenceSnapshot, error) {
	if err := catalog.requireGate(); err != nil {
		return store.DocumentEventEvidenceSnapshot{}, err
	}
	return catalog.snapshot, nil
}

func (catalog *gatedDocumentEventCatalog) PublishDocumentEvents(
	context.Context, store.DocumentEventTarget, string, string, []byte,
) (store.DocumentEventGeneration, error) {
	if err := catalog.requireGate(); err != nil {
		return store.DocumentEventGeneration{}, err
	}
	catalog.publishCalls++
	catalog.pending = false
	return store.DocumentEventGeneration{}, nil
}

func (catalog *gatedDocumentEventCatalog) RecordDocumentEventAttempt(
	context.Context, store.DocumentEventTarget, string, string, []byte,
) error {
	return errors.New("unexpected terminal document event attempt")
}

func (catalog *gatedDocumentEventCatalog) RefreshDocumentEventBuilds(context.Context) error {
	if err := catalog.requireGate(); err != nil {
		return err
	}
	catalog.refreshCalls++
	return nil
}

type documentEventFailureCatalog struct {
	DocumentEventCatalog

	snapshot      store.DocumentEventEvidenceSnapshot
	loadErr       error
	recordedState string
	attempts      int
}

func (catalog *documentEventFailureCatalog) LoadDocumentEventEvidence(
	context.Context, store.DocumentEventTarget,
) (store.DocumentEventEvidenceSnapshot, error) {
	return catalog.snapshot, catalog.loadErr
}

func (catalog *documentEventFailureCatalog) RecordDocumentEventAttempt(
	_ context.Context, _ store.DocumentEventTarget, _ string, state string, _ []byte,
) error {
	catalog.recordedState = state
	catalog.attempts++
	return nil
}

type documentEventStaleOnceCatalog struct {
	*store.Store

	once       sync.Once
	mutate     func()
	cancel     context.CancelFunc
	publishErr error
}

func (catalog *documentEventStaleOnceCatalog) PublishDocumentEvents(
	ctx context.Context,
	target store.DocumentEventTarget,
	fingerprint string,
	inputsSHA256 string,
	canonical []byte,
) (store.DocumentEventGeneration, error) {
	catalog.once.Do(catalog.mutate)
	generation, err := catalog.Store.PublishDocumentEvents(
		ctx, target, fingerprint, inputsSHA256, canonical,
	)
	catalog.publishErr = err
	catalog.cancel()
	return generation, err
}

func openDocumentEventTestStore(t *testing.T) *store.Store {
	t.Helper()
	catalog, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	return catalog
}

func publishDocumentEventTestMetadata(
	t *testing.T,
	catalog *store.Store,
	sourceSHA256 string,
	extractorSeed string,
	fields []document.SourceMetadataFieldV1,
) {
	t.Helper()
	if fields == nil {
		fields = []document.SourceMetadataFieldV1{}
	}
	canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1,
		Fields:          fields,
		Warnings:        []document.SourceMetadataWarningV1{},
	})
	require.NoError(t, err)
	_, err = catalog.PublishSourceMetadata(
		t.Context(), sourceSHA256, testDigest(extractorSeed), canonical,
	)
	require.NoError(t, err)
}
