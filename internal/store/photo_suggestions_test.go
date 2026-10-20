package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

var (
	producerA = strings.Repeat("a", 64)
	producerB = strings.Repeat("b", 64)
)

// putPhotoSuggestion publishes a description for the asset's displayed
// version as automatic work from producer A and returns what the owner sees.
func putPhotoSuggestion(t *testing.T, s *Store, asset PhotoAsset, description document.PhotoDescription) PhotoSuggestion {
	t.Helper()
	publishPhotoDescription(t, s, asset, producerA, false, description)
	values := photoSuggestionValues(t, s, asset.ID)
	require.Len(t, values, 1)
	return values[0]
}

func publishPhotoDescription(t *testing.T, s *Store, asset PhotoAsset, producer string, explicit bool, description document.PhotoDescription) {
	t.Helper()
	ctx := t.Context()
	node, err := s.NodeByID(ctx, fileByID(asset.Files, *asset.DisplayFileID).NodeID)
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return publishPhotoSuggestionTx(ctx, tx, node.CurrentVersionID, producer, explicit, description, time.Now().UTC())
	}))
}

func photoSuggestionValues(t *testing.T, s *Store, assetID string) []PhotoSuggestion {
	t.Helper()
	prepared, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: []string{assetID}})
	require.NoError(t, err)
	return prepared.Suggestions
}

func acceptPhotoSuggestion(t *testing.T, s *Store, proposal PhotoSuggestion, caption bool, tags ...string) PhotoSuggestion {
	t.Helper()
	// Refresh the revisions the accepted edit is checked against.
	prepared, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: []string{assetIDOfSuggestion(t, s, proposal)}})
	require.NoError(t, err)
	for _, current := range prepared.Suggestions {
		if current.ID == proposal.ID {
			proposal = current
		}
	}
	result, err := s.RespondPhotoSuggestion(t.Context(), proposal.ID, PhotoSuggestionResponse{SourceRevision: proposal.SourceRevision, NodeRevision: proposal.NodeRevision, Response: "accepted", Caption: caption, Tags: tags})
	require.NoError(t, err)
	return result
}

func assetIDOfSuggestion(t *testing.T, s *Store, proposal PhotoSuggestion) string {
	t.Helper()
	var assetID string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT asset_id FROM photo_files WHERE file_id=?`, proposal.Target).Scan(&assetID))
	return assetID
}

func restorePhotoSuggestionStore(t *testing.T, s *Store) *Store {
	t.Helper()
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &metadata))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	return restored
}

func searchCount(t *testing.T, s *Store, text string, sort string) int {
	t.Helper()
	return len(browsePhotoPage(t, s, fmt.Sprintf(`{"syntax":"simple","text":%q,"sort":{"field":%q,"direction":"desc"}}`, text, sort)).Items)
}

func TestAcceptedTagStaysSearchableAfterRenameAndReplacement(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "synthetic.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Tags: []string{"coast", "sunset"}})
	acceptPhotoSuggestion(t, s, proposal, false, "coast")
	for _, sort := range []string{"name", "relevance"} {
		require.Equal(t, 1, searchCount(t, s, "coast", sort), "accepting a tag makes it authored text")
	}

	tag, err := s.TagByName(ctx, "coast")
	require.NoError(t, err)
	_, err = s.RenameTag(ctx, tag.ID, tag.Revision, "shore")
	require.NoError(t, err)
	for _, sort := range []string{"name", "relevance"} {
		require.Zero(t, searchCount(t, s, "coast", sort), "the old name no longer matches")
		require.Equal(t, 1, searchCount(t, s, "shore", sort), "a rename moves the match with the tag")
	}

	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("ab"), 10, "image/jpeg")
	require.NoError(t, err)
	require.Equal(t, 1, searchCount(t, s, "shore", "name"), "replacing the content keeps assigned tags searchable")
}

func TestPhotoSearchPhraseStaysInsideOneValue(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := albumAsset(t, s, "phrases.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "harbor", Tags: []string{"red boat", "blue sky"}, VisibleText: "QUAYSIDE"})
	phrase := func(text string) int {
		return len(browsePhotoPage(t, s, fmt.Sprintf(`{"syntax":"advanced","text":%q}`, `"`+text+`"`)).Items)
	}
	require.Equal(t, 1, phrase("red boat"))
	require.Zero(t, phrase("boat blue"), "a phrase cannot span two suggested tags")
	require.Zero(t, phrase("harbor QUAYSIDE"), "a phrase cannot span caption and visible text")
	require.Zero(t, phrase("QUAYSIDE red"), "a phrase cannot span visible text and tags")
	acceptPhotoSuggestion(t, s, proposal, false, "red boat", "blue sky")
	require.Equal(t, 1, phrase("blue sky"))
	require.Zero(t, phrase("boat blue"), "a phrase cannot span two assigned tags")
}

func TestHandAddedTagsMatchTextSearch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := albumAsset(t, s, "hand-tagged.jpg")
	hand, err := s.CreateTag(t.Context(), "handtag")
	require.NoError(t, err)
	_, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Keywords: []PhotoKeywordEdit{{TagID: hand.ID, Assign: true}}})
	require.NoError(t, err)
	for _, sort := range []string{"name", "relevance"} {
		require.Equal(t, 1, searchCount(t, s, "handtag", sort), "a tag matches however it was assigned")
	}
	current, err := s.TagByName(t.Context(), "handtag")
	require.NoError(t, err)
	_, err = s.DeleteTag(t.Context(), current.ID, current.Revision)
	require.NoError(t, err)
	require.Zero(t, searchCount(t, s, "handtag", "name"), "deleting a tag removes its text")
}

func TestHiddenPhotoSuggestionRequiresUnlock(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "hidden.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A photo.", Tags: []string{}, VisibleText: "QUAYSIDE"})
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.Zero(t, searchCount(t, s, "QUAYSIDE", "relevance"), "hidden photos stay out of ordinary search")
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "dismissed"})
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	page, err := s.ListPhotoAssets(unlocked, PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"text":"QUAYSIDE"}`), Hidden: true}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1, "an unlocked hidden browse finds suggestion text")
	_, err = s.RespondPhotoSuggestion(unlocked, proposal.ID, PhotoSuggestionResponse{Response: "dismissed"})
	require.NoError(t, err)
}

func TestPhotoSuggestionAcceptsSelectedFieldsAndDismissesTheRest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "accept.jpg")
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT 1`).Err())
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast", "sunset"}, VisibleText: "QUAYSIDE"})

	_, err := s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "accepted"})
	require.ErrorIs(t, err, ErrInvalidPhotoSuggestionResponse, "an acceptance names its fields")
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "dismissed", Caption: true})
	require.ErrorIs(t, err, ErrInvalidPhotoSuggestionResponse, "a dismissal names none")
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{SourceRevision: proposal.SourceRevision, NodeRevision: proposal.NodeRevision, Response: "accepted", Tags: []string{"other"}})
	require.ErrorIs(t, err, ErrPhotoSuggestionConflict, "a tag that was never suggested is not pending")

	accepted := acceptPhotoSuggestion(t, s, proposal, true, "coast")
	require.Equal(t, document.PhotoDescription{Tags: []string{"sunset"}, VisibleText: "QUAYSIDE"}, accepted.Value)
	require.NotNil(t, accepted.AuthoredReceipt)
	prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	require.Equal(t, "A harbor.", prepared.Targets[0].Values.Caption)
	require.Len(t, prepared.Targets[0].Tags, 1)
	require.Equal(t, accepted.Value, prepared.Suggestions[0].Value, "what remains stays pending")

	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{SourceRevision: accepted.SourceRevision, NodeRevision: accepted.NodeRevision, Response: "accepted", Caption: true})
	require.ErrorIs(t, err, ErrPhotoSuggestionConflict, "an accepted field is no longer pending")
	for _, term := range []string{"sunset", "QUAYSIDE"} {
		require.Equal(t, 1, searchCount(t, s, term, "relevance"))
	}

	dismissed, err := s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "dismissed"})
	require.NoError(t, err)
	require.Empty(t, dismissed.Value.Tags)
	require.Empty(t, photoSuggestionValues(t, s, asset.ID))
	for _, term := range []string{"sunset", "QUAYSIDE"} {
		require.Zero(t, searchCount(t, s, term, "relevance"), "dismissal removes the text from search")
	}
	require.Equal(t, 1, searchCount(t, s, "coast", "relevance"), "accepted fields stay authored")
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "dismissed"})
	require.ErrorIs(t, err, ErrPhotoSuggestionConflict)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoSuggestionRanksBelowAuthoredTextAndMarksItself(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	authored := albumAsset(t, s, "authored.jpg")
	machine := albumAsset(t, s, "machine.jpg")
	_, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{authored.ID}}), Patch: PhotoAuthoredPatch{Caption: new("harbor")}})
	require.NoError(t, err)
	putPhotoSuggestion(t, s, machine, document.PhotoDescription{Caption: "harbor"})

	page := browsePhotoPage(t, s, `{"syntax":"simple","text":"harbor","sort":{"field":"relevance","direction":"desc"}}`)
	require.Len(t, page.Items, 2)
	require.Equal(t, authored.ID, page.Items[0].AssetID, "authored text ranks first")
	require.False(t, page.Items[0].MatchedAISuggestion)
	require.Equal(t, machine.ID, page.Items[1].AssetID)
	require.True(t, page.Items[1].MatchedAISuggestion)

	page = browsePhotoPage(t, s, `{"syntax":"simple","text":"harbor","sort":{"field":"name","direction":"asc"}}`)
	require.Len(t, page.Items, 2, "other sorts still find suggestion text")
	for _, item := range page.Items {
		require.False(t, item.MatchedAISuggestion, "only relevance order shows the mark")
	}
	page = browsePhotoPage(t, s, `{"syntax":"simple","text":"machine","sort":{"field":"relevance","direction":"desc"}}`)
	require.Len(t, page.Items, 1)
	require.False(t, page.Items[0].MatchedAISuggestion, "a file name match wins over suggestion text")
}

func TestPhotoSuggestionBackupKeepsDecisionsAndRegeneratesOnlyOpenFields(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "restored.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast", "sunset", "boats"}, VisibleText: "QUAYSIDE"})
	acceptPhotoSuggestion(t, s, proposal, true, "coast")
	// The owner rewrites the accepted caption and removes the accepted tag.
	_, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Caption: new("My caption.")}})
	require.NoError(t, err)
	coast, err := s.TagByName(ctx, "coast")
	require.NoError(t, err)
	_, err = s.UnassignTag(ctx, coast.ID, asset.Files[0].NodeID, UnconditionalRev)
	require.NoError(t, err)

	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	for _, term := range []string{"sunset", "boats", "QUAYSIDE"} {
		require.NotContains(t, backup.String(), term, "pending text is not backed up")
	}
	require.Equal(t, 1, strings.Count(backup.String(), `"type":"photo_suggestion"`))

	restored := restorePhotoSuggestionStore(t, s)
	require.NoError(t, restored.ValidateMetadata(ctx))
	require.Empty(t, photoSuggestionValues(t, restored, asset.ID), "nothing is shown until the description is regenerated")
	publishPhotoDescription(t, restored, asset, producerA, false, document.PhotoDescription{Caption: "Regenerated.", Tags: []string{"coast", "sunset", "harbor"}, VisibleText: "NEW TEXT"})
	regenerated := photoSuggestionValues(t, restored, asset.ID)
	require.Len(t, regenerated, 1)
	require.Equal(t, document.PhotoDescription{Tags: []string{"sunset", "harbor"}, VisibleText: "NEW TEXT"}, regenerated[0].Value,
		"the resolved caption and the removed tag are not offered again")
}

func TestPhotoSuggestionBackupKeepsDismissal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "dismissed.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor."})
	_, err := s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "dismissed"})
	require.NoError(t, err)
	restored := restorePhotoSuggestionStore(t, s)
	publishPhotoDescription(t, restored, asset, producerA, false, document.PhotoDescription{Caption: "Again."})
	require.Empty(t, photoSuggestionValues(t, restored, asset.ID), "a dismissed description is not asked for again")
	settled, err := restored.PhotoDescriptionSettled(ctx, proposal.ContentVersionID, "")
	require.NoError(t, err)
	require.True(t, settled)
}

func TestPhotoDescriptionPredicates(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "predicates.jpg")
	version := func() string {
		node, err := s.NodeByID(ctx, fileByID(asset.Files, *asset.DisplayFileID).NodeID)
		require.NoError(t, err)
		return node.CurrentVersionID
	}()
	settled := func(explicitProducer string) bool {
		value, err := s.PhotoDescriptionSettled(ctx, version, explicitProducer)
		require.NoError(t, err)
		return value
	}
	require.False(t, settled(""), "a version without rows wants a description")
	require.False(t, settled(producerB))

	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "From A."})
	require.True(t, settled(""), "a live suggestion settles automatic work")
	require.True(t, settled(producerB), "another profile does not replace a live suggestion")

	_, err := s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "dismissed"})
	require.NoError(t, err)
	require.True(t, settled(""), "a changed default profile never re-describes a dismissal")
	require.True(t, settled(producerA), "the producer of a dismissal is not asked again")
	require.False(t, settled(producerB), "an explicit start with another profile replaces a dismissal")

	publishPhotoDescription(t, s, asset, producerB, false, document.PhotoDescription{Caption: "Automatic B."})
	require.Empty(t, photoSuggestionValues(t, s, asset.ID), "automatic work never publishes over a dismissal")
	publishPhotoDescription(t, s, asset, producerB, true, document.PhotoDescription{Caption: "Explicit B."})
	current := photoSuggestionValues(t, s, asset.ID)
	require.Len(t, current, 1)
	require.Equal(t, "Explicit B.", current[0].Value.Caption)
	require.True(t, settled(producerB))
	require.True(t, settled(producerA), "one live suggestion settles every producer")
}

func TestPhotoDescriptionSkipsFieldsAlreadyAuthored(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "authored-first.jpg")
	tag, err := s.CreateTag(ctx, "coast")
	require.NoError(t, err)
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Caption: new("A harbor.")}, Keywords: []PhotoKeywordEdit{{TagID: tag.ID, Assign: true}}})
	require.NoError(t, err)
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast", "sunset"}})
	require.Equal(t, document.PhotoDescription{Tags: []string{"sunset"}}, proposal.Value)
}

// photoDescriptionJobRequest builds the queue request a description job
// carries for the version's displayed photo.
func photoDescriptionJobRequest(t *testing.T, s *Store, versionID, producer string, explicit bool) RenditionJobRequest {
	t.Helper()
	ctx := t.Context()
	version, err := s.ContentVersionByID(ctx, versionID)
	require.NoError(t, err)
	recipe, err := document.BuiltInVisualPreviewRecipe("grid")
	require.NoError(t, err)
	preview, err := s.PublishVisualPreviewGeneration(ctx, version.ID, readyVisualPreviewWithRecipe(t, version.BlobHash, fakeHash("c3"), 9, recipe), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)
	profile := catalogProcessingProfileWith(t, false, func(p *document.ProcessingProfileV1) {
		p.Rendition.AdapterContract = document.VisionAdapterContract
		p.Rendition.DeploymentFingerprint = producer
		p.RetentionDisclosure.RetainSanitizedMarkdown = false
		p.RetentionDisclosure.RetainTypedArtifacts = false
	})
	request := renditionJobTestRequest(version.ID, profile)
	request.ExecutionIdentity.Authorization.SourceSHA256 = version.BlobHash
	request.ExecutionIdentity.Upload.SHA256 = version.BlobHash
	request.CapturedArtifactPolicy = []byte(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"}],"version":1}`)
	request.Authorization.InputClasses = []string{"derived_upload"}
	request.Authorization.RetainedArtifactClasses = []string{"normalized_evidence"}
	request.ExecutionIdentity.Upload.InputKind = document.RenditionInputDerivedUpload
	request.ExecutionIdentity.Upload.InputBinding = PhotoDescriptionInputBinding(version.ID, preview.Checksum, explicit)
	request.ExecutionIdentity.Authorization.InputKind = document.RenditionInputDerivedUpload
	return request
}

func photoDescriptionResult(caption string) *document.RenditionResult {
	return &document.RenditionResult{Artifacts: []document.RenditionArtifact{{Role: document.EvidenceArtifactStructured, Payload: []byte(`{"caption":"` + caption + `","tags":[],"visible_text":""}`)}}}
}

// startPhotoDescription enqueues the request and begins its provider call.
func startPhotoDescription(t *testing.T, s *Store, request RenditionJobRequest, at time.Time) (RenditionJob, RenditionJobWaiter, RenditionJobClaim) {
	t.Helper()
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "description-worker", at, time.Hour)
	require.NoError(t, err)
	_, err = s.BeginRenditionProvider(t.Context(), claim, waiter.ID, at, renditionJobTestSnapshot(request))
	require.NoError(t, err)
	return job, waiter, claim
}

func TestPhotoDescriptionJobPublishesOneSuggestionAndSettles(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "job.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	request := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
	grantRenditionJobConsent(t, s, request)
	at := time.Now().UTC().Add(time.Minute)
	_, _, claim := startPhotoDescription(t, s, request, at)
	publication, err := s.PublishPhotoDescription(ctx, claim, photoDescriptionResult("A harbor."), at)
	require.NoError(t, err)
	require.Equal(t, 1, publication.PublishedWaiterCount)
	require.Equal(t, "A harbor.", photoSuggestionValues(t, s, asset.ID)[0].Value.Caption)
	require.NoError(t, s.ValidateMetadata(ctx))

	joined, waiter, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	require.Equal(t, RenditionJobCompleted, joined.State, "a settled version needs no second provider call")
	require.Equal(t, "published", waiter.State)

	_, err = s.PublishRenditionJob(ctx, claim, at)
	require.Error(t, err, "a description job only publishes through its own path")
	_, err = s.PublishPhotoDescription(ctx, claim, nil, at)
	require.ErrorIs(t, err, ErrInvalidPhotoDescriptionResult)
}

func TestPhotoDescriptionEmptyReplyIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "empty.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	request := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
	grantRenditionJobConsent(t, s, request)
	at := time.Now().UTC().Add(time.Minute)
	_, _, claim := startPhotoDescription(t, s, request, at)
	_, err = s.PublishPhotoDescription(ctx, claim, photoDescriptionResult(""), at)
	require.NoError(t, err)
	require.Empty(t, photoSuggestionValues(t, s, asset.ID))
	settled, err := s.PhotoDescriptionSettled(ctx, node.CurrentVersionID, "")
	require.NoError(t, err)
	require.True(t, settled, "an empty answer still settles the version")
}

func TestPhotoDescriptionExpiredGrantOverlappedByRenewalPublishesNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "renewed.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	request := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
	expiresAt := time.Now().UTC().Add(time.Minute)
	_, err = s.GrantConsent(ctx, grantRequestForAuthorization(request.Authorization, &expiresAt))
	require.NoError(t, err)
	job, waiter, claim := startPhotoDescription(t, s, request, time.Now().UTC().Add(10*time.Second))

	grantRenditionJobConsent(t, s, request)
	joined, renewed, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	require.Equal(t, job.ID, joined.ID)
	require.NotEqual(t, waiter.ID, renewed.ID)

	_, err = s.PublishPhotoDescription(ctx, claim, photoDescriptionResult("Late caption."), expiresAt.Add(time.Second))
	require.ErrorIs(t, err, ErrProcessingConsentExpired)
	require.Empty(t, photoSuggestionValues(t, s, asset.ID))
	require.NoError(t, s.MarkRenditionJobFailed(ctx, claim, RenditionFailureConsent, expiresAt.Add(time.Second)))
	requeued, _, err := s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)
	require.Equal(t, RenditionJobQueued, requeued.State, "the discarded answer is asked for again")
}

func TestPhotoDescriptionAnswerForPhotoHiddenAtPublishRetriesWhenVisible(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "hide.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	request := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
	grantRenditionJobConsent(t, s, request)
	at := time.Now().UTC().Add(time.Minute)
	_, _, claim := startPhotoDescription(t, s, request, at)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	hidden, err := s.SetPhotoAssetHidden(unlocked, asset.ID, asset.Revision, true)
	require.NoError(t, err)

	_, err = s.PublishPhotoDescription(ctx, claim, photoDescriptionResult("Hidden."), at)
	require.ErrorIs(t, err, ErrRenditionJobStaleAuthority)
	prepared, err := s.PreparePhotoAuthored(unlocked, PhotoSetSelection{AssetIDs: []string{asset.ID}, Hidden: true})
	require.NoError(t, err)
	require.Empty(t, prepared.Suggestions)
	require.NoError(t, s.MarkRenditionJobFailed(ctx, claim, RenditionFailureStaleAuthority, at))

	_, err = s.SetPhotoAssetHidden(unlocked, asset.ID, hidden.Revision, false)
	require.NoError(t, err)
	_, _, claim = startPhotoDescription(t, s, request, at.Add(time.Second))
	_, err = s.PublishPhotoDescription(ctx, claim, photoDescriptionResult("Visible."), at.Add(time.Second))
	require.NoError(t, err)
	prepared, err = s.PreparePhotoAuthored(unlocked, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	require.Equal(t, "Visible.", prepared.Suggestions[0].Value.Caption)
}

func TestExplicitPhotoDescriptionReplacesOnlyADismissal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "explicit.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "From A."})
	explicit := photoDescriptionJobRequest(t, s, proposal.ContentVersionID, producerB, true)
	grantRenditionJobConsent(t, s, explicit)
	at := time.Now().UTC().Add(time.Minute)

	_, waiter, err := s.EnqueueRenditionJob(ctx, explicit)
	require.NoError(t, err)
	require.Equal(t, "published", waiter.State, "a live suggestion is not replaced")

	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "dismissed"})
	require.NoError(t, err)
	automatic := photoDescriptionJobRequest(t, s, proposal.ContentVersionID, producerB, false)
	grantRenditionJobConsent(t, s, automatic)
	_, waiter, err = s.EnqueueRenditionJob(ctx, automatic)
	require.NoError(t, err)
	require.Equal(t, "published", waiter.State, "automatic work leaves a dismissal alone")

	_, waiter, claim := startPhotoDescription(t, s, explicit, at)
	require.Equal(t, "waiting", waiter.State, "an explicit start by another profile asks again")
	_, err = s.PublishPhotoDescription(ctx, claim, photoDescriptionResult("From B."), at)
	require.NoError(t, err)
	require.Equal(t, "From B.", photoSuggestionValues(t, s, asset.ID)[0].Value.Caption)
}

func TestDerivativePurgeLeavesPhotoSuggestionsButVersionPurgeRemovesThem(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "purge.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor."})
	_, err := s.PurgeDerivatives(ctx, PurgeRequest{All: true})
	require.NoError(t, err)
	require.Len(t, photoSuggestionValues(t, s, asset.ID), 1, "dismissal is how a description is removed")

	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "dismissed"})
	require.NoError(t, err)
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	_, err = s.TrashPhotoAsset(ctx, asset.ID, asset.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	require.NotContains(t, exported.String(), proposal.ID, "purging the photo removes its decision record")
}
