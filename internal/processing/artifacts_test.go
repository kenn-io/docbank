package processing

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Test coverage for explicitly auxiliary interoperability metadata.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

var errInjectedPublication = errors.New("injected publication failure")

func TestPublishRenditionPublishesVerifiedArtifactsAndHeads(t *testing.T) {
	// Mutation caught: omitting physical membership or either head publication
	// would leave the successful result unreadable through its public stores.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)

	published, err := publisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "searchable mercury evidence", "first markdown",
	))
	require.NoError(t, err)
	assert.Equal(t, processingHash("b1"), published.BuildID)
	assert.Equal(t, processingHash("51"), published.AttachmentID)
	assert.Equal(t, processingHash("91"), published.LexicalGeneration.ID)
	require.Len(t, published.Artifacts, 2)
	for _, artifact := range published.Artifacts {
		physical, err := fixture.catalog.PhysicalContent(t.Context(), artifact.Hash)
		require.NoError(t, err)
		assert.Equal(t, artifact.Size, physical.LogicalBytes)
	}
	view, err := fixture.catalog.ActiveRendition(
		t.Context(), fixture.versionID, fixture.profile.Fingerprint,
	)
	require.NoError(t, err)
	for _, artifact := range view.Build.Artifacts {
		checksum, checksumErr := fixture.catalog.BlobChecksums(t.Context(), artifact.BlobHash)
		if artifact.Role == "sanitized_markdown" {
			require.NoError(t, checksumErr)
			assert.Equal(t, artifact.MD5, checksum.MD5)
			assert.Len(t, artifact.MD5, 32)
		} else {
			require.ErrorIs(t, checksumErr, store.ErrNotFound)
			assert.Empty(t, artifact.MD5)
		}
	}

	assert.Equal(t, published.BuildID, view.Build.ID)
	active, err := fixture.catalog.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, published.LexicalGeneration, active)
	hits, _, err := fixture.catalog.SearchPage(t.Context(), "mercury", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, fixture.versionID, hits[0].Node.CurrentVersionID)
	assert.Equal(t, store.SearchMatchContent, hits[0].Match)
}

func TestPublishRenditionExactRetryIgnoresDerivedMD5(t *testing.T) {
	// Mutation caught: loading the first publication hydrates the Markdown MD5,
	// which must not change the immutable build declaration used by a retry.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	ids := publicationIDs{"b1", "51", "91"}

	first := fixture.stage(t, ids, "searchable mercury evidence", "first markdown")
	first.Build.Warnings = nil
	_, err = publisher.PublishRendition(t.Context(), first)
	require.NoError(t, err)
	retry := fixture.stage(t, ids, "searchable mercury evidence", "first markdown")
	retry.Build.Warnings = nil
	_, err = publisher.PublishRendition(t.Context(), retry)
	require.NoError(t, err, "an exact retry must reuse the immutable build")
}

func TestPublishRenditionAcceptsNilBuildWarnings(t *testing.T) {
	// Mutation caught: exact slice comparison rejects a warning-free build
	// when one representation uses nil and the other uses an empty slice.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	staged := fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "searchable mercury evidence", "first markdown",
	)
	require.Empty(t, staged.Rendition.Warnings)
	staged.Build.Warnings = nil

	_, err = publisher.PublishRendition(t.Context(), staged)
	require.NoError(t, err)
}

func TestAuxiliaryChecksumBackfillResumesAndReadsPackedOnlyContent(t *testing.T) {
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })

	payloads := [][]byte{[]byte("first legacy original"), []byte("second legacy original")}
	for index, payload := range payloads {
		receipt, writeErr := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(payload))
		require.NoError(t, writeErr)
		encoding, encodingErr := receipt.EncodingName()
		require.NoError(t, encodingErr)
		_, createErr := catalog.CreateFile(t.Context(), catalog.RootID(),
			"legacy-"+strconv.Itoa(index)+".bin", receipt.Hash, receipt.Size,
			"application/octet-stream", store.BlobPhysical{
				Encoding: encoding, StoredBytes: receipt.StoredSize,
				PackEligible: receipt.PackEligible, Created: receipt.Created,
			})
		require.NoError(t, createErr)
	}
	packed, err := blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, packed.BlobsPacked)

	ctx, cancel := context.WithCancel(t.Context())
	interrupted := &cancelOnSecondChecksumOpen{delegate: blobs, cancel: cancel}
	completed, err := BackfillAuxiliaryChecksums(ctx, catalog, interrupted, 10)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, completed)

	completed, err = BackfillAuxiliaryChecksums(t.Context(), catalog, blobs, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, completed)
	completed, err = BackfillAuxiliaryChecksums(t.Context(), catalog, blobs, 10)
	require.NoError(t, err)
	assert.Zero(t, completed, "completed rows must not be recomputed")

	for _, payload := range payloads {
		sha := sha256.Sum256(payload)
		record, lookupErr := catalog.BlobChecksums(t.Context(), hex.EncodeToString(sha[:]))
		require.NoError(t, lookupErr)
		want := md5.Sum(payload) //nolint:gosec // Explicit auxiliary MD5 assertion.
		assert.Equal(t, hex.EncodeToString(want[:]), record.MD5)
	}
}

type cancelOnSecondChecksumOpen struct {
	delegate verifiedBlobReader
	cancel   context.CancelFunc
	calls    int
}

func (r *cancelOnSecondChecksumOpen) OpenStreamContext(
	ctx context.Context, hash string,
) (packstore.VerifiedReadCloser, int64, error) {
	r.calls++
	if r.calls == 2 {
		r.cancel()
	}
	return r.delegate.OpenStreamContext(ctx, hash)
}

func TestPublishRenditionRejectsLexicalSegmentLimitOutsideCanonicalProfile(t *testing.T) {
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	staged := fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "short searchable evidence", "short markdown",
	)
	normalization, err := document.NewNormalizePolicy(100_000)
	require.NoError(t, err)
	staged.RenditionPolicy, err = document.NewRenditionPolicy(normalization, 4_000)
	require.NoError(t, err)

	_, err = publisher.PublishRendition(t.Context(), staged)
	require.ErrorContains(t, err, "canonical profile max segment runes")
}

func TestPublishRenditionRejectsArtifactReceiptMismatchBeforeCatalogAuthority(t *testing.T) {
	// Mutation caught: trusting declared size instead of the verified CAS
	// receipt would grant catalog authority to bytes from a rejected candidate.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	staged := fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "searchable mercury evidence", "first markdown",
	)
	actualHash := staged.Build.Artifacts[0].BlobHash
	staged.Build.Artifacts[0].Size++

	_, err = publisher.PublishRendition(t.Context(), staged)
	require.Error(t, err)
	snapshot, err := fixture.catalog.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	var authorized int
	require.NoError(t, snapshot.QueryRowContext(t.Context(), store.BackupBlobAuthorityCTE+
		`SELECT COUNT(*) FROM backup_authorized_blobs WHERE hash=?`, actualHash).Scan(&authorized))
	require.NoError(t, snapshot.Close())
	assert.Zero(t, authorized, "a rejected receipt must remain non-backup staging")
	purged, err := fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{})
	require.NoError(t, err)
	assert.Contains(t, purged.PhysicalDerivativeBlobsPendingGC, actualHash,
		"abandoned receipt bytes must remain discoverable for exact physical erasure")
	_, err = fixture.catalog.ActiveRendition(
		t.Context(), fixture.versionID, fixture.profile.Fingerprint,
	)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestPublishRenditionRejectsRetentionAndConcreteProfileBoundsBeforeWriting(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		want   string
		mutate func(*testing.T, *StagedRendition)
	}{
		{
			name: "invalid captured artifact policy",
			want: "captured artifact policy fingerprint",
			mutate: func(_ *testing.T, staged *StagedRendition) {
				staged.Build.CapturedArtifactPolicy = jsontext.Value(`{"roles":[],"version":1}`)
			},
		},
		{
			name: "sanitized markdown retention disabled",
			want: "sanitized Markdown retention",
			mutate: func(t *testing.T, staged *StagedRendition) {
				t.Helper()
				updateStagedProfile(t, staged, func(profile *document.ProcessingProfileV1) {
					profile.RetentionDisclosure.RetainSanitizedMarkdown = false
				})
			},
		},
		{
			name: "typed artifact retention disabled",
			want: "typed artifact retention",
			mutate: func(t *testing.T, staged *StagedRendition) {
				t.Helper()
				updateStagedProfile(t, staged, func(profile *document.ProcessingProfileV1) {
					profile.RetentionDisclosure.RetainTypedArtifacts = false
				})
				staged.Build.Artifacts[0].Role = string(document.EvidenceArtifactStructured)
				policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"sanitized_markdown"},{"max_count":1,"min_count":1,"role":"structured_evidence"}],"version":1}`)
				staged.Build.CapturedArtifactPolicy = policy
				staged.Build.CapturedArtifactPolicyFingerprint = processingSHA256(policy)
			},
		},
		{
			name: "provider markdown retention disabled",
			want: "provider Markdown retention",
			mutate: func(_ *testing.T, staged *StagedRendition) {
				staged.Build.Artifacts[0].Role = string(document.EvidenceArtifactMarkdown)
				policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"provider_markdown"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
				staged.Build.CapturedArtifactPolicy = policy
				staged.Build.CapturedArtifactPolicyFingerprint = processingSHA256(policy)
			},
		},
		{
			name: "declared response bytes",
			want: "max response bytes",
			mutate: func(t *testing.T, staged *StagedRendition) {
				t.Helper()
				updateStagedProfile(t, staged, func(profile *document.ProcessingProfileV1) {
					profile.Rendition.MaxResponseBytes = 1
				})
			},
		},
		{
			name: "unit rune bound",
			want: "max unit runes",
			mutate: func(t *testing.T, staged *StagedRendition) {
				t.Helper()
				updateStagedProfile(t, staged, func(profile *document.ProcessingProfileV1) {
					profile.EvidenceLexical.MaxUnitRunes = 100
				})
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
			require.NoError(t, err)
			staged := fixture.stage(t, publicationIDs{"ba", "5a", "9a"},
				strings.Repeat("x", 101), "bounded markdown")
			testCase.mutate(t, &staged)

			_, err = publisher.PublishRendition(t.Context(), staged)
			require.ErrorContains(t, err, testCase.want)
			for _, artifact := range staged.Build.Artifacts {
				_, lookupErr := fixture.catalog.PhysicalContent(t.Context(), artifact.BlobHash)
				require.ErrorIs(t, lookupErr, store.ErrNotFound,
					"profile and captured-policy failures must precede retained-byte writes")
			}
		})
	}
}

func TestPublishRenditionStopsReadingArtifactPastDeclaredSize(t *testing.T) {
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	staged := fixture.stage(t, publicationIDs{"bc", "5c", "9c"}, "bounded evidence", "bounded markdown")
	oversized := bytes.NewReader(append(bytes.Repeat([]byte("x"),
		int(staged.Build.Artifacts[0].Size)+1), bytes.Repeat([]byte("unread"), 1<<16)...))
	staged.Artifacts[0].Payload = oversized

	_, err = publisher.PublishRendition(t.Context(), staged)
	require.ErrorContains(t, err, "does not match catalog")
	assert.Greater(t, oversized.Len(), 1<<16,
		"publication must stop after the declared artifact size plus one byte")
}

func TestPublishRenditionRejectsForgedProducerGraph(t *testing.T) {
	// Mutation caught: trusting caller-supplied rendition, unit, and segment
	// checksums allows unrelated evidence, Markdown, and searchable text to be
	// staged as one immutable build.
	for _, testCase := range []struct {
		name   string
		want   string
		mutate func(*testing.T, *StagedRendition)
	}{
		{
			name: "noncanonical normalized evidence bytes with forged checksums",
			want: "not exact canonical bytes",
			mutate: func(t *testing.T, staged *StagedRendition) {
				t.Helper()
				evidenceBytes, err := io.ReadAll(staged.Artifacts[0].Payload)
				require.NoError(t, err)
				evidenceBytes = append(evidenceBytes, '\n')
				evidenceHash := processingSHA256(evidenceBytes)
				staged.Artifacts[0].Payload = bytes.NewReader(evidenceBytes)
				staged.Build.Artifacts[0].BlobHash = evidenceHash
				staged.Build.Artifacts[0].Checksum = evidenceHash
				staged.Build.Artifacts[0].Size = int64(len(evidenceBytes))
				staged.Rendition.EvidenceChecksum = evidenceHash
				staged.Build.EvidenceChecksum = evidenceHash
				staged.Rendition.Checksum = processingRenditionChecksum(staged.Rendition)
				staged.Build.RenditionChecksum = staged.Rendition.Checksum
			},
		},
		{
			name: "search text unrelated to rendition unit with forged checksums",
			want: "does not match deterministic producer output",
			mutate: func(t *testing.T, staged *StagedRendition) {
				t.Helper()
				segment := &staged.Rendition.LexicalSegments[0]
				segment.Text = "forged searchable text"
				segment.CharEnd = len([]rune(segment.Text))
				segment.Checksum = processingChecksumStrings(
					document.RenditionContractV1, segment.UnitID, strconv.Itoa(segment.Order),
					strconv.Itoa(segment.CharStart), strconv.Itoa(segment.CharEnd), segment.Text,
				)
				segment.ID = "lexical_segment_" + segment.Checksum
				staged.Build.LexicalSegments[0] = store.RenditionLexicalSegmentRecord{
					ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order,
					CharStart: segment.CharStart, CharEnd: segment.CharEnd,
					Checksum: segment.Checksum, Text: segment.Text,
				}
				staged.Rendition.Checksum = processingRenditionChecksum(staged.Rendition)
				staged.Build.RenditionChecksum = staged.Rendition.Checksum
			},
		},
		{
			name: "self consistent rendition unrelated to producer output",
			want: "does not match deterministic producer output",
			mutate: func(t *testing.T, staged *StagedRendition) {
				t.Helper()
				unit := &staged.Rendition.Units[0]
				unit.Text = "fully forged but internally consistent text"
				unit.Checksum = processingChecksumStrings(
					document.RenditionContractV1, unit.EvidenceUnitID, strconv.Itoa(unit.Order),
					unit.Text, strings.Join(unit.HeadingPath, "\x00"),
				)
				unit.ID = "rendition_unit_" + unit.Checksum
				staged.Build.Units[0] = store.RenditionUnitRecord{
					ID: unit.ID, EvidenceUnitID: unit.EvidenceUnitID, Order: unit.Order,
					Checksum: unit.Checksum, HeadingPath: append([]string(nil), unit.HeadingPath...),
					Locator: unit.Locator,
				}
				staged.Rendition.Markdown = []byte(unit.Text + "\n")
				staged.Rendition.MarkdownChecksum = processingSHA256(staged.Rendition.Markdown)
				staged.Build.MarkdownChecksum = staged.Rendition.MarkdownChecksum
				staged.Artifacts[1].Payload = bytes.NewReader(staged.Rendition.Markdown)
				staged.Build.Artifacts[1].BlobHash = staged.Rendition.MarkdownChecksum
				staged.Build.Artifacts[1].Checksum = staged.Rendition.MarkdownChecksum
				staged.Build.Artifacts[1].Size = int64(len(staged.Rendition.Markdown))

				segment := &staged.Rendition.LexicalSegments[0]
				segment.UnitID = unit.ID
				segment.Text = unit.Text
				segment.CharEnd = len([]rune(segment.Text))
				segment.Checksum = processingChecksumStrings(
					document.RenditionContractV1, segment.UnitID, strconv.Itoa(segment.Order),
					strconv.Itoa(segment.CharStart), strconv.Itoa(segment.CharEnd), segment.Text,
				)
				segment.ID = "lexical_segment_" + segment.Checksum
				staged.Build.LexicalSegments[0] = store.RenditionLexicalSegmentRecord{
					ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order,
					CharStart: segment.CharStart, CharEnd: segment.CharEnd,
					Checksum: segment.Checksum, Text: segment.Text,
				}
				staged.Rendition.Checksum = processingRenditionChecksum(staged.Rendition)
				staged.Build.RenditionChecksum = staged.Rendition.Checksum
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
			require.NoError(t, err)
			staged := fixture.stage(t,
				publicationIDs{"bf", "5f", "9f"}, "canonical searchable text", "canonical heading",
			)
			testCase.mutate(t, &staged)

			_, err = publisher.PublishRendition(t.Context(), staged)
			require.ErrorContains(t, err, testCase.want)
			purged, purgeErr := fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{})
			require.NoError(t, purgeErr)
			for _, artifact := range staged.Build.Artifacts {
				assert.Contains(t, purged.PhysicalDerivativeBlobsPendingGC, artifact.BlobHash,
					"graph rejection must leave every written receipt under exact erasure authority")
			}
			_, err = fixture.catalog.ActiveRendition(
				t.Context(), fixture.versionID, fixture.profile.Fingerprint,
			)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestPublishRenditionFailureAfterBlobClosePreservesPriorHeads(t *testing.T) {
	// Mutation caught: continuing after a blob writer reports a terminal-close
	// failure would make an unverified retained payload reachable.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	first, err := publisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "prior mercury evidence", "first markdown",
	))
	require.NoError(t, err)

	failingWriter := &failAfterClosedBlobWriter{Store: fixture.blobs}
	failingPublisher, err := NewArtifactPublisher(fixture.catalog, failingWriter)
	require.NoError(t, err)
	_, err = failingPublisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"b2", "52", "92"}, "replacement venus evidence", "second markdown",
	))
	require.ErrorIs(t, err, errInjectedPublication)
	assertPriorPublicationServes(t, fixture, first)
}

func TestPublishRenditionFailureAfterCatalogStagePreservesPriorHeads(t *testing.T) {
	// Mutation caught: treating a staged immutable build as published would let
	// catalog membership bypass version-scoped attachment and head authority.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	first, err := publisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "prior mercury evidence", "first markdown",
	))
	require.NoError(t, err)

	failingCatalog := &failAfterCatalogStage{renditionPublicationCatalog: fixture.catalog}
	failingPublisher, err := NewArtifactPublisher(failingCatalog, fixture.blobs)
	require.NoError(t, err)
	second := fixture.replacementStage(t,
		publicationIDs{"b2", "52", "92"}, "replacement venus evidence", "second markdown",
	)
	_, err = failingPublisher.PublishRendition(t.Context(), second)
	require.ErrorIs(t, err, errInjectedPublication)
	assertPriorPublicationServes(t, fixture, first)

	assert.True(t, failingCatalog.staged,
		"the injected failure occurs after the immutable build is staged")
}

func TestPublishRenditionExcludesDerivativePurgeAcrossEveryStagingBoundary(t *testing.T) {
	for _, boundary := range []string{"receipt", "build", "lexical"} {
		t.Run(boundary, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			blocking := &blockingPublicationCatalog{
				renditionPublicationCatalog: fixture.catalog,
				boundary:                    boundary, reached: make(chan struct{}), release: make(chan struct{}),
			}
			publisher, err := NewArtifactPublisher(blocking, fixture.blobs)
			require.NoError(t, err)
			staged := fixture.stage(t, publicationIDs{"bc", "5c", "9c"},
				"concurrent synthetic evidence", "concurrent markdown")
			publicationDone := make(chan error, 1)
			go func() {
				_, publishErr := publisher.PublishRendition(t.Context(), staged)
				publicationDone <- publishErr
			}()
			<-blocking.reached

			purgeDone := make(chan error, 1)
			go func() {
				_, purgeErr := maintenance.PurgeDerivatives(
					t.Context(), fixture.catalog, fixture.blobs, store.PurgeRequest{})
				purgeDone <- purgeErr
			}()
			require.Eventually(t, func() bool {
				probeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
				defer cancel()
				return errors.Is(fixture.blobs.WithMutation(probeCtx, func() error { return nil }),
					context.DeadlineExceeded)
			}, time.Second, time.Millisecond,
				"purge must queue exclusive maintenance behind the in-flight publication")
			select {
			case purgeErr := <-purgeDone:
				require.Failf(t, "purge completed during publication", "error: %v", purgeErr)
			default:
			}

			close(blocking.release)
			require.NoError(t, <-publicationDone)
			require.NoError(t, <-purgeDone)
			view, err := fixture.catalog.ActiveRendition(
				t.Context(), fixture.versionID, fixture.profile.Fingerprint)
			require.NoError(t, err)
			assert.Equal(t, staged.Build.ID, view.Build.ID)
		})
	}
}

type publicationFixture struct {
	catalog         *store.Store
	blobs           *blob.Store
	profile         store.ProcessingProfileRecord
	evidencePolicy  document.EvidencePolicy
	renditionPolicy document.RenditionPolicy
	versionID       string
}

type publicationIDs struct {
	build, attachment, generation string
}

func newPublicationFixture(t *testing.T) publicationFixture {
	t.Helper()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })

	source := []byte("synthetic private-free source")
	receipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
	require.NoError(t, err)
	physical := processingBlobPhysical(t, receipt)
	node, err := catalog.CreateFile(
		t.Context(), catalog.RootID(), "source.pdf", receipt.Hash, receipt.Size,
		"application/pdf", physical,
	)
	require.NoError(t, err)
	evidencePolicy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	normalizationPolicy, err := document.NewNormalizePolicy(100_000)
	require.NoError(t, err)
	renditionPolicy, err := document.NewRenditionPolicy(normalizationPolicy, 100)
	require.NoError(t, err)
	return publicationFixture{
		catalog: catalog, blobs: blobs, profile: processingProfile(t),
		evidencePolicy: evidencePolicy, renditionPolicy: renditionPolicy,
		versionID: node.CurrentVersionID,
	}
}

func (f publicationFixture) stage(
	t *testing.T,
	ids publicationIDs, lexicalText, markdown string,
) StagedRendition {
	t.Helper()
	return f.stageForSource(t, ids, lexicalText, markdown, f.versionID, f.mustSourceHash())
}

func (f publicationFixture) replacementStage(
	t *testing.T, ids publicationIDs, lexicalText, markdown string,
) StagedRendition {
	t.Helper()
	source := []byte("replacement source " + ids.build)
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(
		t.Context(), f.catalog.RootID(), "replacement-"+ids.build+".pdf",
		receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt),
	)
	require.NoError(t, err)
	return f.stageForSource(t, ids, lexicalText, markdown, node.CurrentVersionID, receipt.Hash)
}

func (f publicationFixture) stageForSource(
	t *testing.T,
	ids publicationIDs, lexicalText, markdown, versionID, sourceHash string,
) StagedRendition {
	t.Helper()
	normalizedEvidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceComplete,
		Family:          "pdf",
		UnitKind:        document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0, Text: lexicalText, HeadingPath: []string{markdown},
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
				Start: 1, End: 1,
			},
		}},
	}, f.evidencePolicy)
	require.NoError(t, err)
	evidence, evidenceHash, err := document.MarshalNormalizedEvidenceV1(normalizedEvidence)
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(normalizedEvidence, f.renditionPolicy)
	require.NoError(t, err)
	markdownBytes := rendition.Markdown
	markdownHash := rendition.MarkdownChecksum
	policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	warnings := make([]string, len(rendition.Warnings))
	for index, warning := range rendition.Warnings {
		warnings[index] = warning.Code
	}
	units := make([]store.RenditionUnitRecord, len(rendition.Units))
	for index, unit := range rendition.Units {
		units[index] = store.RenditionUnitRecord{
			ID: unit.ID, EvidenceUnitID: unit.EvidenceUnitID, Order: unit.Order,
			Checksum: unit.Checksum, HeadingPath: append([]string(nil), unit.HeadingPath...),
			Locator: unit.Locator,
		}
	}
	segments := make([]store.RenditionLexicalSegmentRecord, len(rendition.LexicalSegments))
	for index, segment := range rendition.LexicalSegments {
		segments[index] = store.RenditionLexicalSegmentRecord{
			ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order,
			CharStart: segment.CharStart, CharEnd: segment.CharEnd,
			Checksum: segment.Checksum, Text: segment.Text,
		}
	}
	build := store.RenditionBuildRecord{
		ID: processingHash(ids.build), VaultID: f.catalog.VaultID(),
		SourceSHA256:                      sourceHash,
		RenditionRequestFingerprint:       f.profile.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        f.profile.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: processingSHA256(policy),
		CapturedArtifactPolicy:            policy, AuthorizationChecksum: processingHash(ids.build + "a6"),
		ProviderOperationID: "synthetic-operation-" + ids.build,
		ProviderReceipt:     jsontext.Value(`{"provider":"synthetic","request_id":"` + ids.build + `"}`),
		EvidenceChecksum:    evidenceHash, RenditionChecksum: rendition.Checksum,
		MarkdownChecksum: markdownHash, Completeness: document.EvidenceComplete,
		Warnings: warnings, CompletedAt: "2026-08-22T09:00:00.000000000Z",
		DeclaredArtifactCount: 2,
		Artifacts: []store.RenditionArtifactRecord{
			{ID: "artifact_" + processingHash(ids.build+"21"), Role: "normalized_evidence", BlobHash: evidenceHash,
				Size: int64(len(evidence)), Checksum: evidenceHash, State: store.RenditionArtifactVerified},
			{ID: "artifact_" + processingHash(ids.build+"22"), Role: "sanitized_markdown", BlobHash: markdownHash,
				Size: int64(len(markdownBytes)), Checksum: markdownHash, State: store.RenditionArtifactVerified},
		},
		Units: units, LexicalSegments: segments,
	}
	attachment := store.RenditionAttachmentRecord{
		ID: processingHash(ids.attachment), VaultID: f.catalog.VaultID(),
		ContentVersionID: versionID, BuildID: build.ID, Profile: f.profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	return StagedRendition{
		Rendition: rendition, RenditionPolicy: f.renditionPolicy,
		Build: build, Attachment: attachment,
		Head: store.RenditionHeadRecord{
			ContentVersionID: versionID, ProcessingProfileFingerprint: f.profile.Fingerprint,
			AttachmentID: attachment.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z",
		},
		LexicalGenerationID: processingHash(ids.generation),
		Artifacts: []StagedArtifact{
			{ID: build.Artifacts[0].ID, Payload: bytes.NewReader(evidence)},
			{ID: build.Artifacts[1].ID, Payload: bytes.NewReader(markdownBytes)},
		},
	}
}

func (f publicationFixture) mustSourceHash() string {
	node, err := f.catalog.NodeByPath(context.Background(), "/source.pdf")
	if err != nil {
		panic(err)
	}
	return node.BlobHash
}

func processingProfile(t *testing.T) store.ProcessingProfileRecord {
	t.Helper()
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "rendition-adapter/v1", AuthorizationFingerprint: processingHash("a1"),
			CredentialBinding: "credential:synthetic", DeploymentFingerprint: processingHash("a2"),
			Descriptor:            document.ProviderDescriptorV1{ID: "synthetic-rendition", Fingerprint: processingHash("a3")},
			DisclosureFingerprint: processingHash("a4"), MaxDocumentBytes: 1 << 20,
			MaxResponseBytes: 1 << 20, MaxUnits: 100, Name: "primary",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      "synthetic-vault", UploadOptionsFingerprint: processingHash("a5"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: processingHash("b1"), LexicalSegmenterFingerprint: processingHash("b2"),
			MaxDocumentChars: 100_000, MaxSegmentRunes: 100, MaxUnitRunes: 1000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      processingHash("b3"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: processingHash("b4"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: processingHash("c1"), ConsentFingerprint: processingHash("c2"),
			RetainSanitizedMarkdown: true, RetainTypedArtifacts: true, TrustBoundary: "synthetic-vault",
		},
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
}

func updateStagedProfile(
	t *testing.T, staged *StagedRendition, mutate func(*document.ProcessingProfileV1),
) {
	t.Helper()
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(
		staged.Attachment.Profile.CanonicalProfile, &profile, json.RejectUnknownMembers(true)))
	mutate(&profile)
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	staged.Attachment.Profile = store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
	staged.Head.ProcessingProfileFingerprint = fingerprints.Profile
	staged.Build.RenditionRequestFingerprint = fingerprints.RenditionRequest
	staged.Build.EvidenceLexicalFingerprint = fingerprints.EvidenceLexical
}

func processingBlobPhysical(t *testing.T, receipt blob.WriteReceipt) store.BlobPhysical {
	t.Helper()
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	return store.BlobPhysical{
		Encoding: encoding, StoredBytes: receipt.StoredSize,
		PackEligible: receipt.PackEligible, MD5: receipt.MD5, Created: receipt.Created,
	}
}

type failAfterClosedBlobWriter struct {
	*blob.Store

	failed bool
}

func (w *failAfterClosedBlobWriter) WriteDetailedContext(
	ctx context.Context, reader io.Reader,
) (blob.WriteReceipt, error) {
	receipt, err := w.Store.WriteDetailedContext(ctx, reader)
	if err == nil && !w.failed {
		w.failed = true
		return receipt, errInjectedPublication
	}
	return receipt, err
}

type failAfterCatalogStage struct {
	renditionPublicationCatalog

	staged bool
}

type blockingPublicationCatalog struct {
	renditionPublicationCatalog

	boundary string
	reached  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (c *blockingPublicationCatalog) block(boundary string) {
	if c.boundary != boundary {
		return
	}
	c.once.Do(func() {
		close(c.reached)
		<-c.release
	})
}

func (c *blockingPublicationCatalog) RecordRenditionBlob(
	ctx context.Context, hash string, size int64, physical store.BlobPhysical,
) error {
	if err := c.renditionPublicationCatalog.RecordRenditionBlob(
		ctx, hash, size, physical); err != nil {
		return err
	}
	c.block("receipt")
	return nil
}

func (c *blockingPublicationCatalog) StageRenditionBuild(
	ctx context.Context, record store.RenditionBuildRecord,
) error {
	if err := c.renditionPublicationCatalog.StageRenditionBuild(ctx, record); err != nil {
		return err
	}
	c.block("build")
	return nil
}

func (c *blockingPublicationCatalog) StageLexicalGeneration(
	ctx context.Context, generationID string,
) (store.LexicalGeneration, error) {
	generation, err := c.renditionPublicationCatalog.StageLexicalGeneration(ctx, generationID)
	if err != nil {
		return store.LexicalGeneration{}, err
	}
	c.block("lexical")
	return generation, nil
}

func (c *failAfterCatalogStage) StageRenditionBuild(
	ctx context.Context, record store.RenditionBuildRecord,
) error {
	if err := c.renditionPublicationCatalog.StageRenditionBuild(ctx, record); err != nil {
		return err
	}
	c.staged = true
	return errInjectedPublication
}

func assertPriorPublicationServes(
	t *testing.T, fixture publicationFixture, prior PublishedRendition,
) {
	t.Helper()
	view, err := fixture.catalog.ActiveRendition(
		t.Context(), fixture.versionID, fixture.profile.Fingerprint,
	)
	require.NoError(t, err)
	assert.Equal(t, prior.BuildID, view.Build.ID)
	active, err := fixture.catalog.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, prior.LexicalGeneration, active)
	hits, _, err := fixture.catalog.SearchPage(t.Context(), "mercury", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	hits, _, err = fixture.catalog.SearchPage(t.Context(), "venus", 10)
	require.NoError(t, err)
	assert.Empty(t, hits)
}

func processingHash(seed string) string { return processingSHA256([]byte(seed)) }

func processingSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func processingChecksumStrings(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(hash, strconv.Itoa(len(value)))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func processingRenditionChecksum(rendition document.RenditionV1) string {
	parts := []string{
		document.RenditionContractV1, rendition.EvidenceChecksum,
		string(rendition.Completeness), rendition.MarkdownChecksum,
	}
	for _, unit := range rendition.Units {
		parts = append(parts, unit.Checksum)
	}
	for _, segment := range rendition.LexicalSegments {
		parts = append(parts, segment.Checksum)
	}
	for _, warning := range rendition.Warnings {
		parts = append(parts, warning.Code)
	}
	return processingChecksumStrings(parts...)
}
