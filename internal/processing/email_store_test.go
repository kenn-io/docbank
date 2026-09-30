package processing

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/emailmime"
	"go.kenn.io/docbank/internal/store"
)

const emailStoreSource = "From: Sender <sender@example.test>\r\nSubject: Headeronlyterm\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nChosen body mercury."

type emailStoreFixture struct {
	publicationFixture

	email     store.EmailMetadataView
	inventory store.EmailPublication
	payloads  map[string][]byte
}

// EmailStoreRestoreTestFixture shares the email fixture with external backup tests.
type EmailStoreRestoreTestFixture struct {
	Catalog     *store.Store
	Blobs       *blob.Store
	Email       store.EmailMetadataView
	Payloads    map[string][]byte
	PublishBody func() string
}

func NewEmailStoreRestoreTestFixture(t *testing.T) EmailStoreRestoreTestFixture {
	t.Helper()
	f := newEmailStoreFixture(t)
	return EmailStoreRestoreTestFixture{
		Catalog: f.catalog, Blobs: f.blobs, Email: f.email, Payloads: f.payloads,
		PublishBody: func() string {
			published, err := f.publisher(t, nil).PublishRendition(t.Context(), f.bodyStage(t))
			require.NoError(t, err)
			return published.BuildID
		},
	}
}

func newEmailStoreFixture(t *testing.T) emailStoreFixture {
	t.Helper()
	f := emailStoreFixture{publicationFixture: newPublicationFixture(t), payloads: map[string][]byte{}}
	source := []byte(emailStoreSource)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	decoded, err := emailmime.Decode(t.Context(), processingSHA256(source), int64(len(source)), bytes.NewReader(source), root)
	require.NoError(t, err)
	defer func() { require.NoError(t, decoded.Close()) }()
	require.NoError(t, f.blobs.WithMutation(t.Context(), func() error {
		wr, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
		if err != nil {
			return err
		}
		n, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "mail.eml", wr.Hash, wr.Size, "message/rfc822", processingBlobPhysical(t, wr))
		if err != nil {
			return err
		}
		f.versionID = n.CurrentVersionID
		f.inventory.ContentVersionID = f.versionID
		f.inventory.CanonicalJSON, _, err = document.MarshalEmailV1(decoded.Evidence)
		if err != nil {
			return err
		}
		for _, a := range decoded.Artifacts() {
			r, err := decoded.OpenArtifact(t.Context(), a.PartPath, string(a.Reference.Role))
			if err != nil {
				return err
			}
			b, err := io.ReadAll(r)
			closeErr := r.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			wr, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(b))
			if err != nil {
				return err
			}
			if err = f.catalog.RecordRenditionBlob(t.Context(), wr.Hash, wr.Size, processingBlobPhysical(t, wr)); err != nil {
				return err
			}
			f.payloads[wr.Hash] = b
			f.inventory.Artifacts = append(f.inventory.Artifacts, store.EmailPartArtifactRecord{PartPath: a.PartPath, Role: string(a.Reference.Role), BlobSHA256: wr.Hash, Size: wr.Size})
		}
		f.email, err = f.catalog.PublishEmailGeneration(t.Context(), f.inventory)
		return err
	}))
	p, err := document.EmailBodyProfileV1(f.email.Evidence.Recipe)
	require.NoError(t, err)
	canonical, fp, err := document.CanonicalProfile(p)
	require.NoError(t, err)
	f.profile = store.ProcessingProfileRecord{Fingerprint: fp.Profile, CanonicalProfile: canonical, RenditionRequestFingerprint: fp.RenditionRequest, EvidenceLexicalFingerprint: fp.EvidenceLexical, RetentionDisclosureFingerprint: fp.RetentionDisclosure, AttachmentPolicyFingerprint: p.RetentionDisclosure.AttachmentPolicyFingerprint, ConsentFingerprint: p.RetentionDisclosure.ConsentFingerprint, RenditionDisclosureFingerprint: p.Rendition.DisclosureFingerprint, TrustBoundary: p.RetentionDisclosure.TrustBoundary}
	f.evidencePolicy, err = document.NewEvidencePolicy(64 << 20)
	require.NoError(t, err)
	f.renditionPolicy, err = document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 64 << 20, MaxUnitRunes: 4_000_000, MaxSegmentRunes: 4096})
	require.NoError(t, err)
	_, err = f.catalog.MigrateLegacyPlainText(t.Context())
	require.NoError(t, err)
	return f
}

func (f emailStoreFixture) bodyStage(t *testing.T) StagedRendition {
	t.Helper()
	normalized, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceDegradedProvenance, Family: "text", UnitKind: document.EvidenceUnitGeneric, Omissions: []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: "Selected body converted to derived generic text blocks; exact MIME authority retained separately."}}, Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: "```\nChosen body mercury.\n```", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone}}}}, f.evidencePolicy)
	require.NoError(t, err)
	evidence, evidenceHash, err := document.MarshalNormalizedEvidenceV1(normalized)
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(normalized, f.renditionPolicy)
	require.NoError(t, err)
	body := f.email.Evidence.Inventory.Parts[0].BodyUTF8
	recipe, err := document.EmailBodyRecipeFingerprint(f.email.Evidence.Recipe)
	require.NoError(t, err)
	receipt := document.EmailBodyReceiptV1{ContractVersion: document.EmailBodyReceiptContractV1, SourceSHA256: f.email.Version.BlobHash, SourceSize: f.email.Version.Size, EmailGenerationID: f.email.Generation.ID, EmailChecksum: f.email.Generation.Checksum, PartPath: "1", BodySHA256: body.SHA256, BodySize: body.Size, BodyRecipeFingerprint: recipe, EvidenceChecksum: evidenceHash, RenditionChecksum: rendition.Checksum, MarkdownChecksum: rendition.MarkdownChecksum}
	receiptJSON, _, err := document.MarshalEmailBodyReceiptV1(receipt)
	require.NoError(t, err)
	buildID, err := document.EmailBodyBuildID(receipt)
	require.NoError(t, err)
	operation, err := document.EmailBodyOperationID(recipe)
	require.NoError(t, err)
	auth, err := document.EmailBodyAuthorizationChecksum(receipt.SourceSHA256, receipt.SourceSize, recipe)
	require.NoError(t, err)
	policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	build := store.RenditionBuildRecord{ID: buildID, VaultID: f.catalog.VaultID(), SourceSHA256: f.email.Version.BlobHash, RenditionRequestFingerprint: f.profile.RenditionRequestFingerprint, EvidenceLexicalFingerprint: f.profile.EvidenceLexicalFingerprint, CapturedArtifactPolicyFingerprint: processingSHA256(policy), CapturedArtifactPolicy: policy, AuthorizationChecksum: auth, ProviderOperationID: operation, ProviderReceipt: receiptJSON, EvidenceChecksum: evidenceHash, RenditionChecksum: rendition.Checksum, MarkdownChecksum: rendition.MarkdownChecksum, Completeness: rendition.Completeness, CompletedAt: "2026-09-09T10:00:00.000000000Z", DeclaredArtifactCount: 2, Artifacts: []store.RenditionArtifactRecord{{ID: "artifact_" + processingHash(buildID+"evidence"), Role: "normalized_evidence", BlobHash: evidenceHash, Checksum: evidenceHash, Size: int64(len(evidence)), State: store.RenditionArtifactVerified}, {ID: "artifact_" + processingHash(buildID+"markdown"), Role: "sanitized_markdown", BlobHash: rendition.MarkdownChecksum, Checksum: rendition.MarkdownChecksum, Size: int64(len(rendition.Markdown)), State: store.RenditionArtifactVerified}}}
	for _, u := range rendition.Units {
		build.Units = append(build.Units, store.RenditionUnitRecord{ID: u.ID, EvidenceUnitID: u.EvidenceUnitID, Order: u.Order, Checksum: u.Checksum, HeadingPath: u.HeadingPath, Locator: u.Locator})
	}
	for _, s := range rendition.LexicalSegments {
		build.LexicalSegments = append(build.LexicalSegments, store.RenditionLexicalSegmentRecord{ID: s.ID, UnitID: s.UnitID, Order: s.Order, CharStart: s.CharStart, CharEnd: s.CharEnd, Checksum: s.Checksum, Text: s.Text})
	}
	for _, w := range rendition.Warnings {
		build.Warnings = append(build.Warnings, w.Code)
	}
	attachment := store.RenditionAttachmentRecord{ID: processingHash(f.versionID + buildID + f.profile.Fingerprint), VaultID: f.catalog.VaultID(), ContentVersionID: f.versionID, BuildID: buildID, Profile: f.profile, AttachedAt: "2026-09-09T10:01:00.000000000Z"}
	return StagedRendition{Rendition: rendition, RenditionPolicy: f.renditionPolicy, Build: build, Attachment: attachment, Head: store.RenditionHeadRecord{ContentVersionID: f.versionID, ProcessingProfileFingerprint: f.profile.Fingerprint, AttachmentID: attachment.ID, PublishedAt: attachment.AttachedAt}, LexicalGenerationID: processingHash(buildID + "projection"), Artifacts: []StagedArtifact{{ID: build.Artifacts[0].ID, Payload: bytes.NewReader(evidence)}, {ID: build.Artifacts[1].ID, Payload: bytes.NewReader(rendition.Markdown)}}}
}

type emailStoreAdapter struct {
	*store.Store

	emailID, recipe, path string
	fail                  bool
	mutate                func(*store.EmailBodyPublication)
}

func (a emailStoreAdapter) PublishRenditionAndLexicalHeads(ctx context.Context, attachment store.RenditionAttachmentRecord, head store.RenditionHeadRecord, generationID string) error {
	if a.fail {
		return errors.New("synthetic transient publication failure")
	}
	p := store.EmailBodyPublication{EmailAttachmentID: a.emailID, PartPath: a.path, BodyRecipeFingerprint: a.recipe, RenditionAttachment: attachment, RenditionHead: head, LexicalGenerationID: generationID}
	if a.mutate != nil {
		a.mutate(&p)
	}
	return a.PublishEmailBody(ctx, p)
}
func (f emailStoreFixture) publisher(t *testing.T, adapt func(*emailStoreAdapter)) *ArtifactPublisher {
	t.Helper()
	recipe, err := document.EmailBodyRecipeFingerprint(f.email.Evidence.Recipe)
	require.NoError(t, err)
	a := emailStoreAdapter{Store: f.catalog, emailID: f.email.Attachment.ID, recipe: recipe, path: "1"}
	if adapt != nil {
		adapt(&a)
	}
	p, err := NewArtifactPublisher(a, f.blobs)
	require.NoError(t, err)
	return p
}

func TestEmailStoreBodyPublisherAtomicRetryAndAssociation(t *testing.T) {
	t.Parallel()
	f := newEmailStoreFixture(t)
	_, err := f.publisher(t, func(a *emailStoreAdapter) { a.fail = true }).PublishRendition(t.Context(), f.bodyStage(t))
	require.ErrorContains(t, err, "transient")
	view, err := f.catalog.EmailMetadata(t.Context(), f.versionID)
	require.NoError(t, err)
	require.Equal(t, "pending", view.BodySearch.State)
	recipe, err := document.EmailRecipeFingerprint(f.email.Evidence.Recipe)
	require.NoError(t, err)
	bodyProfileFingerprint, err := EmailBodyProfileFingerprint()
	require.NoError(t, err)
	targets, err := f.catalog.MissingEmailTargetsAfter(
		t.Context(), recipe, bodyProfileFingerprint, "", 100,
	)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	for _, tc := range []string{"part", "version", "recipe", "head", "lexical"} {
		t.Run(tc, func(t *testing.T) {
			_, err := f.publisher(t, func(a *emailStoreAdapter) {
				a.mutate = func(p *store.EmailBodyPublication) {
					switch tc {
					case "part":
						p.PartPath = "1.2"
					case "version":
						p.RenditionAttachment.ContentVersionID = "00000000-0000-4000-8000-000000000000"
					case "recipe":
						p.BodyRecipeFingerprint = processingHash("wrong recipe")
					case "head":
						p.RenditionHead.AttachmentID = processingHash("wrong head")
					case "lexical":
						p.LexicalGenerationID = processingHash("wrong projection")
					}
				}
			}).PublishRendition(t.Context(), f.bodyStage(t))
			require.Error(t, err)
			v, err := f.catalog.EmailMetadata(t.Context(), f.versionID)
			require.NoError(t, err)
			require.Equal(t, "pending", v.BodySearch.State)
		})
	}
	published, err := f.publisher(t, nil).PublishRendition(t.Context(), f.bodyStage(t))
	require.NoError(t, err)
	view, err = f.catalog.EmailMetadata(t.Context(), f.versionID)
	require.NoError(t, err)
	require.Equal(t, "available", view.BodySearch.State)
	require.Equal(t, published.BuildID, *view.BodySearch.RenditionBuildID)
	hits, _, err := f.catalog.SearchPage(t.Context(), "mercury", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, f.versionID, hits[0].Node.CurrentVersionID)
	hits, _, err = f.catalog.SearchPage(t.Context(), "Headeronlyterm", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
	_, err = f.publisher(t, nil).PublishRendition(t.Context(), f.bodyStage(t))
	require.NoError(t, err)
	require.NoError(t, f.catalog.ValidateMetadata(t.Context()))
	var encoded bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &encoded))
	require.Contains(t, encoded.String(), `"type":"email_body_result"`)
	require.NoError(t, f.catalog.VerifyRenditionBlobBytes(t.Context(), f.blobs))
	_, err = f.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{AttachmentIDs: []string{published.AttachmentID}})
	require.NoError(t, err)
	view, err = f.catalog.EmailMetadata(t.Context(), f.versionID)
	require.NoError(t, err)
	require.Equal(t, "derivative_purged", *view.BodySearch.Reason)
	_, err = f.publisher(t, nil).PublishRendition(t.Context(), f.bodyStage(t))
	require.ErrorContains(t, err, "purge suppression")
	targets, err = f.catalog.MissingEmailTargetsAfter(
		t.Context(), recipe, bodyProfileFingerprint, "", 100,
	)
	require.NoError(t, err)
	require.Empty(t, targets)
}

func TestEmailStoreRejectsReceiptTamperingBeforeHeadPublication(t *testing.T) {
	t.Parallel()
	for _, tc := range []string{"source", "source_size", "generation", "email_checksum", "body", "body_size", "output", "operation", "authorization", "build"} {
		t.Run(tc, func(t *testing.T) {
			f := newEmailStoreFixture(t)
			stage := f.bodyStage(t)
			receipt, _, err := document.DecodeEmailBodyReceiptV1(stage.Build.ProviderReceipt)
			require.NoError(t, err)
			switch tc {
			case "source":
				receipt.SourceSHA256 = processingHash("wrong source")
			case "source_size":
				receipt.SourceSize++
			case "generation":
				receipt.EmailGenerationID = processingHash("wrong email generation")
			case "email_checksum":
				receipt.EmailChecksum = processingHash("wrong email checksum")
			case "body":
				receipt.BodySHA256 = processingHash("wrong body")
			case "body_size":
				receipt.BodySize++
			case "output":
				receipt.EvidenceChecksum = processingHash("wrong evidence")
			}
			stage.Build.ProviderReceipt, _, err = document.MarshalEmailBodyReceiptV1(receipt)
			require.NoError(t, err)
			stage.Build.ID, err = document.EmailBodyBuildID(receipt)
			require.NoError(t, err)
			switch tc {
			case "operation":
				stage.Build.ProviderOperationID = "wrong-operation"
			case "authorization":
				stage.Build.AuthorizationChecksum = processingHash("wrong authority")
			case "build":
				stage.Build.ID = processingHash("wrong build")
			}
			stage.Attachment.BuildID = stage.Build.ID
			_, err = f.publisher(t, nil).PublishRendition(t.Context(), stage)
			require.Error(t, err)
			v, err := f.catalog.EmailMetadata(t.Context(), f.versionID)
			require.NoError(t, err)
			require.Equal(t, "pending", v.BodySearch.State)
			_, err = f.catalog.ActiveRendition(t.Context(), f.versionID, f.profile.Fingerprint)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestEmailStoreChangedInventoryRetiresOnlyItsServingBody(t *testing.T) {
	t.Parallel()
	f := newEmailStoreFixture(t)
	_, err := f.publisher(t, nil).PublishRendition(t.Context(), f.bodyStage(t))
	require.NoError(t, err)
	changed := f.email.Evidence
	changed.Recipe.GoVersion += "-different"
	p := f.inventory
	p.CanonicalJSON, _, err = document.MarshalEmailV1(changed)
	require.NoError(t, err)
	selected, err := f.catalog.PublishEmailGeneration(t.Context(), p)
	require.NoError(t, err)
	require.NotEqual(t, f.email.Generation.ID, selected.Generation.ID)
	_, err = f.catalog.ActiveRendition(t.Context(), f.versionID, f.profile.Fingerprint)
	require.ErrorIs(t, err, store.ErrNotFound)
	historical, err := f.catalog.EmailMetadataGeneration(t.Context(), f.versionID, f.email.Generation.ID)
	require.NoError(t, err)
	require.Equal(t, "unavailable", historical.BodySearch.State)
	require.Equal(t, "superseded", *historical.BodySearch.Reason)
	hits, _, err := f.catalog.SearchPage(t.Context(), "mercury", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
	_, err = f.publisher(t, nil).PublishRendition(t.Context(), f.bodyStage(t))
	require.ErrorContains(t, err, "no longer selected")
	require.NoError(t, f.catalog.ValidateMetadata(t.Context()))
}

func TestEmailStoreFinalPublicationRechecksInventoryPurge(t *testing.T) {
	t.Parallel()
	f := newEmailStoreFixture(t)
	_, err := f.publisher(t, func(a *emailStoreAdapter) {
		a.mutate = func(_ *store.EmailBodyPublication) {
			_, err := f.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{f.versionID}})
			require.NoError(t, err)
		}
	}).PublishRendition(t.Context(), f.bodyStage(t))
	require.Error(t, err)
	_, err = f.catalog.EmailMetadata(t.Context(), f.versionID)
	require.ErrorIs(t, err, store.ErrEmailDerivativeSuppressed)
	_, err = f.catalog.ActiveRendition(t.Context(), f.versionID, f.profile.Fingerprint)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, f.catalog.ValidateMetadata(t.Context()))
}
