package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"slices"
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
	description, err = normalizePhotoDescription(description)
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
	prepared, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: []string{proposal.AssetID}})
	require.NoError(t, err)
	for _, current := range prepared.Suggestions {
		if current.ID == proposal.ID {
			proposal = current
		}
	}
	result, err := s.RespondPhotoSuggestion(t.Context(), proposal.ID, PhotoSuggestionResponse{AssetID: proposal.AssetID, AssetRevision: proposal.AssetRevision, SourceRevision: proposal.SourceRevision, NodeRevision: proposal.NodeRevision, Response: "accepted", Caption: caption, Tags: tags})
	require.NoError(t, err)
	return result
}

// dismissalOf is the dismissal an inspector holding the suggestion's current
// identity and revisions would send.
func dismissalOf(ctx context.Context, t *testing.T, s *Store, suggestion PhotoSuggestion) PhotoSuggestionResponse {
	t.Helper()
	for _, hidden := range []bool{false, true} {
		if prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{suggestion.AssetID}, Hidden: hidden}); err == nil {
			for _, current := range prepared.Suggestions {
				if current.ID == suggestion.ID {
					suggestion = current
				}
			}
		}
	}
	return PhotoSuggestionResponse{AssetID: suggestion.AssetID, AssetRevision: suggestion.AssetRevision, SourceRevision: suggestion.SourceRevision, NodeRevision: suggestion.NodeRevision, Response: "dismissed"}
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
	require.Equal(t, 1, phrase("QUAYSIDE"))
	_, err := s.RespondPhotoSuggestion(t.Context(), proposal.ID, dismissalOf(t.Context(), t, s, proposal))
	require.NoError(t, err)
	require.Zero(t, phrase("QUAYSIDE"), "dismissal removes the text from search")
}

func TestHandAddedTagsMatchTextSearch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "hand-tagged.jpg")
	hand, err := s.CreateTag(ctx, "handtag")
	require.NoError(t, err)
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Keywords: []PhotoKeywordEdit{{TagID: hand.ID, Assign: true}}})
	require.NoError(t, err)
	for _, sort := range []string{"name", "relevance"} {
		require.Equal(t, 1, searchCount(t, s, "handtag", sort), "a tag matches however it was assigned")
	}

	assigned, err := s.TagByName(ctx, "handtag")
	require.NoError(t, err)
	_, err = s.RenameTag(ctx, assigned.ID, assigned.Revision, "shore")
	require.NoError(t, err)
	for _, sort := range []string{"name", "relevance"} {
		require.Zero(t, searchCount(t, s, "handtag", sort), "the old name no longer matches")
		require.Equal(t, 1, searchCount(t, s, "shore", sort), "a rename moves the match with the tag")
	}

	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("ab"), 10, "image/jpeg")
	require.NoError(t, err)
	require.Equal(t, 1, searchCount(t, s, "shore", "name"), "replacing the content keeps assigned tags searchable")

	current, err := s.TagByName(ctx, "shore")
	require.NoError(t, err)
	_, err = s.DeleteTag(ctx, current.ID, current.Revision)
	require.NoError(t, err)
	require.Zero(t, searchCount(t, s, "shore", "name"), "deleting a tag removes its text")
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
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, dismissalOf(ctx, t, s, proposal))
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	page, err := s.ListPhotoAssets(unlocked, PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"text":"QUAYSIDE"}`), Hidden: true}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1, "an unlocked hidden browse finds suggestion text")
	_, err = s.RespondPhotoSuggestion(unlocked, proposal.ID, dismissalOf(unlocked, t, s, proposal))
	require.NoError(t, err)
}

func TestPhotoSuggestionAcceptsSelectedFieldsAndDismissesTheRest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "accept.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast", "sunset"}, VisibleText: "QUAYSIDE"})

	_, err := s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "accepted"})
	require.ErrorIs(t, err, ErrInvalidPhotoSuggestionResponse, "an acceptance names its fields")
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Response: "dismissed", Caption: true})
	require.ErrorIs(t, err, ErrInvalidPhotoSuggestionResponse, "a dismissal names none")
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{AssetID: proposal.AssetID, AssetRevision: proposal.AssetRevision, SourceRevision: proposal.SourceRevision, NodeRevision: proposal.NodeRevision, Response: "accepted", Tags: []string{"other"}})
	require.ErrorIs(t, err, ErrPhotoSuggestionConflict, "a tag that was never suggested is not pending")

	for _, revision := range []string{"asset", "source", "node", "zero source"} {
		request := PhotoSuggestionResponse{AssetID: proposal.AssetID, AssetRevision: proposal.AssetRevision, SourceRevision: proposal.SourceRevision, NodeRevision: proposal.NodeRevision, Response: "accepted", Caption: true, Tags: []string{"coast"}}
		switch revision {
		case "asset":
			request.AssetRevision++
		case "source":
			request.SourceRevision++
		case "node":
			request.NodeRevision++
		case "zero source":
			request.SourceRevision = 0
		}
		_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, request)
		require.ErrorIs(t, err, ErrPhotoSuggestionConflict, revision)
	}

	accepted, err := s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{AssetID: proposal.AssetID, AssetRevision: proposal.AssetRevision, SourceRevision: proposal.SourceRevision, NodeRevision: proposal.NodeRevision, Response: "accepted", Caption: true, Tags: []string{"coast"}})
	require.NoError(t, err)
	require.Equal(t, document.PhotoDescription{Tags: []string{"sunset"}, VisibleText: "QUAYSIDE"}, accepted.Value)
	require.NotNil(t, accepted.AuthoredReceipt)
	prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	require.Equal(t, "A harbor.", prepared.Targets[0].Values.Caption)
	require.Len(t, prepared.Targets[0].Tags, 1)
	require.Equal(t, accepted.Value, prepared.Suggestions[0].Value, "what remains stays pending")

	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{AssetID: accepted.AssetID, AssetRevision: accepted.AssetRevision, SourceRevision: accepted.SourceRevision, NodeRevision: accepted.NodeRevision, Response: "accepted", Caption: true})
	require.ErrorIs(t, err, ErrPhotoSuggestionConflict, "an accepted field is no longer pending")

	// The revisions the last response returned authorize the next acceptance.
	next, err := s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{AssetID: accepted.AssetID, AssetRevision: accepted.AssetRevision, SourceRevision: accepted.SourceRevision, NodeRevision: accepted.NodeRevision, Response: "accepted", Tags: []string{"sunset"}})
	require.NoError(t, err)
	require.Equal(t, document.PhotoDescription{Tags: []string{}, VisibleText: "QUAYSIDE"}, next.Value)

	dismissed, err := s.RespondPhotoSuggestion(ctx, proposal.ID, dismissalOf(ctx, t, s, proposal))
	require.NoError(t, err)
	require.Empty(t, dismissed.Value.VisibleText)
	require.Empty(t, photoSuggestionValues(t, s, asset.ID))
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, dismissalOf(ctx, t, s, proposal))
	require.ErrorIs(t, err, ErrPhotoSuggestionConflict)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoSuggestionResponseChecksTheAssetItWasReviewedUnder(t *testing.T) {
	t.Parallel()
	response := func(proposal PhotoSuggestion, kind string) PhotoSuggestionResponse {
		request := PhotoSuggestionResponse{AssetID: proposal.AssetID, AssetRevision: proposal.AssetRevision, SourceRevision: proposal.SourceRevision, NodeRevision: proposal.NodeRevision, Response: kind}
		if kind == "accepted" {
			request.Caption, request.Tags = true, []string{"coast"}
		}
		return request
	}
	dismissal := func(proposal PhotoSuggestion) PhotoSuggestionResponse { return response(proposal, "dismissed") }
	dismissed := func(s *Store, proposal PhotoSuggestion) bool {
		var value bool
		require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT dismissed FROM photo_suggestions WHERE suggestion_id=?`, proposal.ID).Scan(&value))
		return value
	}
	for _, kind := range []string{"accepted", "dismissed"} {
		t.Run("file moved to another photo, "+kind, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			source := albumAsset(t, s, "moved-from.jpg")
			target := albumAsset(t, s, "moved-to.jpg")
			proposal := putPhotoSuggestion(t, s, source, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast"}})
			file := source.Files[0]
			_, err := s.DetachPhotoFile(ctx, source.ID, source.Revision, file.ID, PhotoDetachOptions{})
			require.NoError(t, err)
			_, err = s.AttachPhotoFile(ctx, target.ID, target.Revision, file.NodeID, "", nil)
			require.NoError(t, err)

			_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, response(proposal, kind))
			require.ErrorIs(t, err, ErrPhotoSuggestionConflict, "the owner reviewed the file under another photo")
			require.False(t, dismissed(s, proposal), "the pending text stays")
			prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{target.ID}})
			require.NoError(t, err)
			for _, moved := range prepared.Targets {
				require.Empty(t, moved.Values.Caption, "the photo that now holds the file gained nothing")
				require.Empty(t, moved.Tags)
			}
		})
	}
	t.Run("revisions moved on", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		asset := albumAsset(t, s, "dismiss-stale.jpg")
		proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor."})
		_, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Rating: new(3)}})
		require.NoError(t, err)

		_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, dismissal(proposal))
		require.ErrorIs(t, err, ErrPhotoSuggestionConflict, "the photo changed since the owner looked")
		require.False(t, dismissed(s, proposal))
		prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{asset.ID}})
		require.NoError(t, err)
		require.Len(t, prepared.Suggestions, 1)
		_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, dismissal(prepared.Suggestions[0]))
		require.NoError(t, err, "a current suggestion can be dismissed")
		require.True(t, dismissed(s, proposal))
	})
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
	// Each needs only part of the AND query from its suggestion, or none of it.
	split := albumAsset(t, s, "split.jpg")
	owned := albumAsset(t, s, "owned-both.jpg")
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{split.ID}}), Patch: PhotoAuthoredPatch{Caption: new("quay")}})
	require.NoError(t, err)
	putPhotoSuggestion(t, s, split, document.PhotoDescription{Tags: []string{"boat"}})
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{owned.ID}}), Patch: PhotoAuthoredPatch{Caption: new("We tied the little boat up beside the old stone quay before the storm came in")}})
	require.NoError(t, err)
	putPhotoSuggestion(t, s, owned, document.PhotoDescription{VisibleText: "boat"})

	page := browsePhotoPage(t, s, `{"syntax":"simple","text":"harbor","sort":{"field":"relevance","direction":"desc"}}`)
	require.Len(t, page.Items, 2)
	require.Equal(t, authored.ID, page.Items[0].AssetID, "authored text ranks first")
	require.False(t, page.Items[0].MatchedAISuggestion)
	require.Equal(t, machine.ID, page.Items[1].AssetID)
	require.True(t, page.Items[1].MatchedAISuggestion)

	for _, tc := range []struct{ form, marked string }{
		{`{"syntax":"simple","text":"harbor","sort":{"field":"name","direction":"asc"}}`, machine.ID},
		{`{"syntax":"simple","text":"harbor","sort":{"field":"capture_time","direction":"desc"}}`, machine.ID},
		{`{"syntax":"advanced","text":"\"harbor\" OR name:zzz","sort":{"field":"name","direction":"asc"}}`, machine.ID},
		{`{"syntax":"advanced","text":"\"harbor\" OR name:zzz","sort":{"field":"relevance","direction":"desc"}}`, machine.ID},
		// Only the photo that needs its suggestion to match the whole query carries the mark.
		{`{"syntax":"advanced","text":"quay AND boat","sort":{"field":"relevance","direction":"desc"}}`, split.ID},
		{`{"syntax":"advanced","text":"quay AND boat","sort":{"field":"name","direction":"desc"}}`, split.ID},
		{`{"syntax":"advanced","text":"quay AND boat","sort":{"field":"capture_time","direction":"desc"}}`, split.ID},
	} {
		page = browsePhotoPage(t, s, tc.form)
		require.Len(t, page.Items, 2, tc.form)
		for _, item := range page.Items {
			require.Equal(t, item.AssetID == tc.marked, item.MatchedAISuggestion, "only the suggestion-only match carries the mark: %s", tc.form)
		}
	}

	// The short caption scores higher on "quay" alone than the natural sentence does on both words,
	// so only the suggestion-aware tier keeps the fully authored match first.
	raw := `{"syntax":"advanced","text":"quay AND boat","sort":{"field":"relevance","direction":"desc"}}`
	page = browsePhotoPage(t, s, raw)
	require.Len(t, page.Items, 2)
	require.Equal(t, owned.ID, page.Items[0].AssetID, "a fully authored match ranks above one that needs suggestion text")
	require.Equal(t, split.ID, page.Items[1].AssetID)

	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	snapshot, err := service.Create(ctx, "owner", SnapshotRequest{Photos: true, Query: snapshotTestQuery(t, raw)})
	require.NoError(t, err)
	require.Len(t, snapshot.Rows, 2)
	require.Equal(t, owned.ID, snapshot.Rows[0].AssetID, "a frozen snapshot keeps the same order")
}

func TestPhotoSearchSkipsSuggestionEvaluationAfterDismissal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "harbor.jpg")
	albumAsset(t, s, "harbor-pier.jpg")
	before := map[string][]PhotoBrowseRow{}
	for _, sort := range []string{"relevance", "name"} {
		before[sort] = browsePhotoPage(t, s, fmt.Sprintf(`{"text":"harbor","sort":{"field":%q,"direction":"desc"}}`, sort)).Items
	}
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "harbor"})
	pending, err := photoSuggestionsExist(ctx, s.db)
	require.NoError(t, err)
	require.True(t, pending)
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, dismissalOf(ctx, t, s, proposal))
	require.NoError(t, err)

	pending, err = photoSuggestionsExist(ctx, s.db)
	require.NoError(t, err)
	require.False(t, pending, "dismissal leaves completion authority but no searchable suggestion text")

	for _, sort := range []string{"relevance", "name"} {
		page := browsePhotoPage(t, s, fmt.Sprintf(`{"text":"harbor","sort":{"field":%q,"direction":"desc"}}`, sort))
		require.Equal(t, before[sort], page.Items, "decided suggestions preserve authored ranking and suggestion marks")
	}
}

func TestPhotoSnapshotSearchKeepsItsRowsWhenASuggestionIsDecidedBetweenPages(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	// A full first page of authored matches, then the one photo that matches through its suggestion.
	authored := make(map[string]bool)
	for i := range 50 {
		asset := albumAsset(t, s, fmt.Sprintf("authored-%02d.jpg", i))
		_, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Caption: new("harbor")}})
		require.NoError(t, err)
		authored[asset.ID] = true
	}
	machine := albumAsset(t, s, "machine.jpg")
	proposal := putPhotoSuggestion(t, s, machine, document.PhotoDescription{Caption: "harbor"})

	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	first, err := service.Create(ctx, "owner", SnapshotRequest{Photos: true, PageSize: 50, Query: snapshotTestQuery(t, `{"syntax":"simple","text":"harbor","sort":{"field":"relevance","direction":"desc"}}`)})
	require.NoError(t, err)
	require.Len(t, first.Rows, 50)
	for _, row := range first.Rows {
		require.True(t, authored[row.AssetID], "authored text ranks first")
	}
	require.NotEmpty(t, first.NextCursor)

	// The owner dismisses the suggestion after the first page.
	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, dismissalOf(ctx, t, s, proposal))
	require.NoError(t, err)
	next, err := service.Page(ctx, "owner", first.SnapshotID, first.NextCursor)
	require.NoError(t, err)
	require.Len(t, next.Rows, 1, "the frozen second page still lists the photo that matched through its suggestion")
	require.Equal(t, machine.ID, next.Rows[0].AssetID)
	back, err := service.Page(ctx, "owner", first.SnapshotID, next.PrevCursor)
	require.NoError(t, err)
	require.Equal(t, first.Rows, back.Rows)
}

func TestPhotoSuggestionOffersNothingTheOwnerAlreadyHas(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "owned.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"Coast", "sunset"}})

	tag, err := s.CreateTag(ctx, "Coast")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, asset.Files[0].NodeID, UnconditionalRev)
	require.NoError(t, err)
	offered := photoSuggestionValues(t, s, asset.ID)
	require.Len(t, offered, 1)
	require.Equal(t, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"sunset"}}, offered[0].Value, "a tag assigned by hand is no longer offered")

	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Caption: new("A harbor.")}})
	require.NoError(t, err)
	offered = photoSuggestionValues(t, s, asset.ID)
	require.Len(t, offered, 1)
	require.Equal(t, document.PhotoDescription{Tags: []string{"sunset"}}, offered[0].Value, "a caption typed identically is no longer offered")

	acceptPhotoSuggestion(t, s, proposal, false, "sunset")
	require.Empty(t, photoSuggestionValues(t, s, asset.ID), "nothing left to offer")

	// The same holds when the owner authored the values before the description arrived.
	early := albumAsset(t, s, "authored-first.jpg")
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{early.ID}}), Patch: PhotoAuthoredPatch{Caption: new("A harbor.")}, Keywords: []PhotoKeywordEdit{{TagID: tag.ID, Assign: true}}})
	require.NoError(t, err)
	publishPhotoDescription(t, s, early, producerA, false, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"Coast", "sunset"}})
	// Reading filters owned values again, so check what publication stored.
	var caption, tags string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT caption,tags_json FROM photo_suggestions WHERE content_version_id=(SELECT current_version_id FROM nodes WHERE id=?)`, early.Files[0].NodeID).Scan(&caption, &tags))
	require.Empty(t, caption, "publication leaves out the authored caption")
	require.JSONEq(t, `["sunset"]`, tags, "publication leaves out the assigned tag")
}

func TestPhotoSuggestionKeepsTagsThatDifferInCase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ owned, suggested string }{{"US", "us"}, {"CAFÉ", "café"}} {
		t.Run(tc.owned, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			asset := albumAsset(t, s, "cased.jpg")
			owned, err := s.CreateTag(ctx, tc.owned)
			require.NoError(t, err)
			proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Tags: []string{tc.suggested}})
			_, err = s.AssignTag(ctx, owned.ID, asset.Files[0].NodeID, UnconditionalRev)
			require.NoError(t, err)
			offered := photoSuggestionValues(t, s, asset.ID)
			require.Len(t, offered, 1)
			require.Equal(t, []string{tc.suggested}, offered[0].Value.Tags, "a case variant of an assigned tag is still offered")

			accepted := acceptPhotoSuggestion(t, s, proposal, false, tc.suggested)
			require.NotNil(t, accepted.AuthoredReceipt)
			_, total, err := s.Tags(ctx, 10, 0)
			require.NoError(t, err)
			require.Equal(t, 2, total, "the case variant is its own tag")
			assigned, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{asset.ID}})
			require.NoError(t, err)
			require.Len(t, assigned.Targets[0].Tags, 2)
			require.Contains(t, assigned.Targets[0].Tags, owned.ID)
		})
	}
}

func TestPhotoSuggestionBackupKeepsDecisions(t *testing.T) {
	t.Parallel()
	t.Run("regenerates only open fields", func(t *testing.T) {
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
	})
	t.Run("regenerates through another producer", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		asset := albumAsset(t, s, "switched.jpg")
		proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast", "sunset"}})
		acceptPhotoSuggestion(t, s, proposal, true)

		restored := restorePhotoSuggestionStore(t, s)
		publishPhotoDescription(t, restored, asset, producerB, false, document.PhotoDescription{Caption: "Regenerated.", Tags: []string{"coast", "sunset"}})
		regenerated := photoSuggestionValues(t, restored, asset.ID)
		require.Len(t, regenerated, 1)
		require.Equal(t, document.PhotoDescription{Tags: []string{"coast", "sunset"}}, regenerated[0].Value, "the resolved caption stays resolved")
	})
	t.Run("keeps another producer's unresolved fields awaiting past a dismissed producer", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		asset := albumAsset(t, s, "replaced.jpg")
		freshAsset := albumAsset(t, s, "fresh.jpg")
		first := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "From A."})
		_, err := s.RespondPhotoSuggestion(ctx, first.ID, dismissalOf(ctx, t, s, first))
		require.NoError(t, err)
		publishPhotoDescription(t, s, asset, producerB, true, document.PhotoDescription{Caption: "From B.", Tags: []string{"coast", "sunset"}})
		second := photoSuggestionValues(t, s, asset.ID)
		require.Len(t, second, 1)
		acceptPhotoSuggestion(t, s, second[0], true, "coast")

		restored := restorePhotoSuggestionStore(t, s)
		// Backfill lists the restored photo for B only; the fresh photo wants A.
		version := func(asset PhotoAsset) string {
			node, err := restored.NodeByID(ctx, asset.Files[0].NodeID)
			require.NoError(t, err)
			// Building the request publishes the ready grid preview that makes the photo a target.
			photoDescriptionJobRequest(t, restored, node.CurrentVersionID, producerA, false)
			return node.CurrentVersionID
		}
		restoredVersion, freshVersion := version(asset), version(freshAsset)
		targets := func(producer string, restoredOnly bool) []string {
			page, err := restored.PhotoDescriptionTargets(ctx, producer, producer, "", 10, restoredOnly)
			require.NoError(t, err)
			ids := []string{}
			for _, version := range page {
				ids = append(ids, version.ID)
			}
			slices.Sort(ids)
			return ids
		}
		require.Equal(t, []string{freshVersion}, targets(producerA, false), "A dismissed the restored photo, so only the fresh one wants A")
		require.Empty(t, targets(producerA, true), "nothing is re-run for the producer that was dismissed")
		require.ElementsMatch(t, []string{restoredVersion, freshVersion}, targets(producerB, false))
		require.Equal(t, []string{restoredVersion}, targets(producerB, true), "B is offered its restored photo and no fresh one")

		publishPhotoDescription(t, restored, asset, producerA, false, document.PhotoDescription{Caption: "Again.", Tags: []string{"coast", "sunset", "boats"}})
		require.Empty(t, photoSuggestionValues(t, restored, asset.ID), "A is dismissed on this photo, so its publish does nothing")

		again := restorePhotoSuggestionStore(t, restored)
		publishPhotoDescription(t, again, asset, producerB, false, document.PhotoDescription{Caption: "Regenerated.", Tags: []string{"coast", "sunset", "boats"}})
		regenerated := photoSuggestionValues(t, again, asset.ID)
		require.Len(t, regenerated, 1)
		require.Equal(t, document.PhotoDescription{Tags: []string{"sunset", "boats"}}, regenerated[0].Value, "only B's unresolved fields come back")
		var dismissed int
		require.NoError(t, again.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_suggestions WHERE producer_fingerprint=? AND dismissed`, producerA).Scan(&dismissed))
		require.Equal(t, 1, dismissed, "A's dismissal survives")
		require.NoError(t, again.ValidateMetadata(ctx))
	})
}

func TestPhotoSuggestionBackupKeepsCompletion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		publish document.PhotoDescription
		respond func(*testing.T, *Store, PhotoSuggestion)
		settled bool
	}{
		{"every caption and tag accepted", document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast", "sunset"}},
			func(t *testing.T, s *Store, proposal PhotoSuggestion) {
				t.Helper()
				acceptPhotoSuggestion(t, s, proposal, true, "coast", "sunset")
			}, true},
		{"nothing to review", document.PhotoDescription{}, nil, true},
		{"partly accepted", document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast", "sunset"}},
			func(t *testing.T, s *Store, proposal PhotoSuggestion) {
				t.Helper()
				acceptPhotoSuggestion(t, s, proposal, true, "coast")
			}, false},
		{"visible text left to review", document.PhotoDescription{Caption: "A harbor.", VisibleText: "QUAYSIDE"},
			func(t *testing.T, s *Store, proposal PhotoSuggestion) {
				t.Helper()
				acceptPhotoSuggestion(t, s, proposal, true)
			}, false},
		{"pending only", document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast"}}, nil, false},
		{"dismissed", document.PhotoDescription{Caption: "A harbor."},
			func(t *testing.T, s *Store, proposal PhotoSuggestion) {
				t.Helper()
				_, err := s.RespondPhotoSuggestion(t.Context(), proposal.ID, dismissalOf(t.Context(), t, s, proposal))
				require.NoError(t, err)
			}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			asset := albumAsset(t, s, "completion.jpg")
			publishPhotoDescription(t, s, asset, producerA, false, tc.publish)
			node, err := s.NodeByID(ctx, fileByID(asset.Files, *asset.DisplayFileID).NodeID)
			require.NoError(t, err)
			if tc.respond != nil {
				values := photoSuggestionValues(t, s, asset.ID)
				require.Len(t, values, 1)
				tc.respond(t, s, values[0])
			}
			current := s
			for restore := 1; restore <= 2; restore++ {
				current = restorePhotoSuggestionStore(t, current)
				require.NoError(t, current.ValidateMetadata(ctx))
				settled, err := current.photoDescriptionSettled(ctx, node.CurrentVersionID, producerA, false)
				require.NoError(t, err)
				require.Equal(t, tc.settled, settled, "after restore %d, fresh consent backfills only a description still owed", restore)
			}
			// Fresh consent runs the automatic backfill, which publishes only when the version is owed one.
			publishPhotoDescription(t, current, asset, producerA, false, document.PhotoDescription{Caption: "Again.", Tags: []string{"harbor"}, VisibleText: "NEW"})
			regenerated := photoSuggestionValues(t, current, asset.ID)
			if tc.settled {
				require.Empty(t, regenerated)
			} else {
				require.Len(t, regenerated, 1)
				require.NotEmpty(t, regenerated[0].Value.Tags)
			}
		})
	}
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
		value, err := s.photoDescriptionSettled(ctx, version, explicitProducer, explicitProducer != "")
		require.NoError(t, err)
		return value
	}
	require.False(t, settled(""), "a version without rows wants a description")
	require.False(t, settled(producerB))

	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "From A."})
	require.True(t, settled(""), "a live suggestion settles automatic work")
	require.True(t, settled(producerB), "another profile does not replace a live suggestion")

	_, err := s.RespondPhotoSuggestion(ctx, proposal.ID, dismissalOf(ctx, t, s, proposal))
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
	request.ExecutionIdentity.Upload.InputBinding = PhotoDescriptionInputBinding(version.ID, preview.Checksum)
	request.ExecutionIdentity.Authorization.InputKind = document.RenditionInputDerivedUpload
	request.ExplicitStart = explicit
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

	_, err = s.PublishPhotoDescription(ctx, claim, nil, at)
	require.ErrorIs(t, err, ErrInvalidPhotoDescriptionResult)

	// An empty answer publishes no text but still settles the version.
	empty := albumAsset(t, s, "empty.jpg")
	emptyNode, err := s.NodeByID(ctx, empty.Files[0].NodeID)
	require.NoError(t, err)
	emptyRequest := photoDescriptionJobRequest(t, s, emptyNode.CurrentVersionID, producerB, false)
	grantRenditionJobConsent(t, s, emptyRequest)
	_, _, claim = startPhotoDescription(t, s, emptyRequest, at)
	_, err = s.PublishPhotoDescription(ctx, claim, photoDescriptionResult(""), at)
	require.NoError(t, err)
	require.Empty(t, photoSuggestionValues(t, s, empty.ID))
	settled, err := s.photoDescriptionSettled(ctx, emptyNode.CurrentVersionID, producerB, false)
	require.NoError(t, err)
	require.True(t, settled, "an empty answer still settles the version")
}

func TestPhotoDescriptionJobIsNeitherDocumentProcessingNorRebuildingCoverage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "described.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	request := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
	grantRenditionJobConsent(t, s, request)
	_, _, err = s.EnqueueRenditionJob(ctx, request)
	require.NoError(t, err)

	page, err := s.ListDocuments(ctx, DocumentCatalogQuery{}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Empty(t, page.Items[0].LatestProcessingState, "a photo description attaches no rendition, so it is no processing state")

	snapshot, err := s.ProcessingCoverage(ctx, ProcessingCoverageScope{
		ContentVersionIDs: []string{node.CurrentVersionID}, ProcessingProfileFingerprint: request.Profile.Fingerprint,
	})
	require.NoError(t, err)
	require.Zero(t, snapshot.Renditions.Rebuilding, "a queued description is not a rendition being rebuilt")
}

func TestPhotoDescriptionTargetsReadsOnePageInOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	var versions []string
	for _, name := range []string{"first.jpg", "second.jpg", "third.jpg"} {
		asset := albumAsset(t, s, name)
		node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
		require.NoError(t, err)
		// Building the request publishes the ready grid preview that makes the photo a target.
		photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
		versions = append(versions, node.CurrentVersionID)
	}
	slices.Sort(versions)
	ids := func(page []ContentVersion) []string {
		out := make([]string, len(page))
		for i, version := range page {
			out[i] = version.ID
		}
		return out
	}

	first, err := s.PhotoDescriptionTargets(ctx, producerB, producerB, "", 2, false)
	require.NoError(t, err)
	require.Equal(t, versions[:2], ids(first), "a small page holds the first targets in version order")
	rest, err := s.PhotoDescriptionTargets(ctx, producerB, producerB, versions[1], 2, false)
	require.NoError(t, err)
	require.Equal(t, versions[2:], ids(rest), "the cursor resumes after the last version read")
}

func TestPhotoDescriptionTargetsSkipPhotosWithActiveWorkForAnotherProfile(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "overlap.jpg")
	publishPhotoDescription(t, s, asset, producerB, true, document.PhotoDescription{Caption: "From B.", Tags: []string{"coast"}})
	restored := restorePhotoSuggestionStore(t, s)
	node, err := restored.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)

	// Profile A takes the restored photo, and its provider call is still running.
	requestA := photoDescriptionJobRequest(t, restored, node.CurrentVersionID, producerA, false)
	grantRenditionJobConsent(t, restored, requestA)
	at := time.Now().UTC().Add(time.Minute)
	startPhotoDescription(t, restored, requestA, at)

	requestB := photoDescriptionJobRequest(t, restored, node.CurrentVersionID, producerB, false)
	targets, err := restored.PhotoDescriptionTargets(ctx, requestB.Profile.Fingerprint, producerB, "", 10, true)
	require.NoError(t, err)
	require.Empty(t, targets, "another profile does not describe a photo whose description is already running")
	targets, err = restored.PhotoDescriptionTargets(ctx, requestA.Profile.Fingerprint, producerA, "", 10, false)
	require.NoError(t, err)
	require.Empty(t, targets, "the running profile does not list it again either")
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

func TestPhotoDescriptionAnswerForPhotoHiddenAtPublishIsRefused(t *testing.T) {
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
	_, err = s.SetPhotoAssetHidden(unlocked, asset.ID, asset.Revision, true)
	require.NoError(t, err)

	_, err = s.PublishPhotoDescription(ctx, claim, photoDescriptionResult("Hidden."), at)
	require.ErrorIs(t, err, ErrRenditionJobStaleAuthority)
	prepared, err := s.PreparePhotoAuthored(unlocked, PhotoSetSelection{AssetIDs: []string{asset.ID}, Hidden: true})
	require.NoError(t, err)
	require.Empty(t, prepared.Suggestions)
}

func TestPhotoDescriptionWaitersKeepTheirOwnDismissalRule(t *testing.T) {
	t.Parallel()
	for _, joined := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit_waiter_%t", joined), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			asset := albumAsset(t, s, "shared.jpg")
			node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
			require.NoError(t, err)
			automatic := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
			grantRenditionJobConsent(t, s, automatic)
			at := time.Now().UTC().Add(time.Minute)
			job, _, claim := startPhotoDescription(t, s, automatic, at)
			// Another profile answers and is dismissed while this job runs.
			first := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "From A."})
			_, err = s.RespondPhotoSuggestion(ctx, first.ID, dismissalOf(ctx, t, s, first))
			require.NoError(t, err)
			if joined {
				explicit := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, true)
				sharedJob, _, err := s.EnqueueRenditionJob(ctx, explicit)
				require.NoError(t, err)
				require.Equal(t, job.ID, sharedJob.ID, "an explicit request joins the automatic job")
			}

			_, err = s.PublishPhotoDescription(ctx, claim, photoDescriptionResult("From B."), at)
			require.NoError(t, err)
			published := photoSuggestionValues(t, s, asset.ID)
			if joined {
				require.Len(t, published, 1, "the explicit waiter replaces the dismissal")
				require.Equal(t, "From B.", published[0].Value.Caption)
			} else {
				require.Empty(t, published, "an automatic waiter leaves the dismissal alone")
			}
		})
	}
	t.Run("enqueue", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		asset := albumAsset(t, s, "explicit.jpg")
		proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "From A."})
		explicit := photoDescriptionJobRequest(t, s, proposal.ContentVersionID, producerB, true)
		grantRenditionJobConsent(t, s, explicit)
		at := time.Now().UTC().Add(time.Minute)

		_, err := s.RespondPhotoSuggestion(ctx, proposal.ID, dismissalOf(ctx, t, s, proposal))
		require.NoError(t, err)
		automatic := photoDescriptionJobRequest(t, s, proposal.ContentVersionID, producerB, false)
		grantRenditionJobConsent(t, s, automatic)
		_, waiter, err := s.EnqueueRenditionJob(ctx, automatic)
		require.NoError(t, err)
		require.Equal(t, "published", waiter.State, "automatic work leaves a dismissal alone")

		_, waiter, _ = startPhotoDescription(t, s, explicit, at)
		require.Equal(t, "waiting", waiter.State, "an explicit start by another profile asks again")
	})
	t.Run("explicit then automatic", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ctx := t.Context()
		asset := albumAsset(t, s, "joined.jpg")
		node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
		require.NoError(t, err)
		first := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "From A."})
		_, err = s.RespondPhotoSuggestion(ctx, first.ID, dismissalOf(ctx, t, s, first))
		require.NoError(t, err)

		explicit := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, true)
		grantRenditionJobConsent(t, s, explicit)
		job, waiter, err := s.EnqueueRenditionJob(ctx, explicit)
		require.NoError(t, err)
		// A delayed backfill admission arrives for the same photo and profile.
		automatic := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
		joined, _, err := s.EnqueueRenditionJob(ctx, automatic)
		require.NoError(t, err)
		require.Equal(t, job.ID, joined.ID)

		at := time.Now().UTC().Add(time.Minute)
		claim, err := s.ClaimRenditionJob(ctx, job.ID, "description-worker", at, time.Hour)
		require.NoError(t, err, "the explicit request is still waiting for its description")
		_, err = s.BeginRenditionProvider(ctx, claim, waiter.ID, at, renditionJobTestSnapshot(explicit))
		require.NoError(t, err)
		_, err = s.PublishPhotoDescription(ctx, claim, photoDescriptionResult("From B."), at)
		require.NoError(t, err)
		published := photoSuggestionValues(t, s, asset.ID)
		require.Len(t, published, 1, "the explicit request replaces the dismissal")
		require.Equal(t, "From B.", published[0].Value.Caption)
	})
}

func TestPhotoDescriptionLeaseExpiryRetriesInsteadOfQuarantining(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "lease.jpg")
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	request := photoDescriptionJobRequest(t, s, node.CurrentVersionID, producerB, false)
	grantRenditionJobConsent(t, s, request)
	at := time.Now().UTC().Add(time.Minute)
	job, _, _ := startPhotoDescription(t, s, request, at)
	// The daemon stops mid call and the lease runs out. A description has no side effects, so the next claim just runs it again.
	_, err = s.ClaimRenditionJob(ctx, job.ID, "restarted-worker", at.Add(2*time.Hour), time.Hour)
	require.NoError(t, err)
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

	_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, dismissalOf(ctx, t, s, proposal))
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
