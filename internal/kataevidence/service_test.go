package kataevidence

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/kataclient"
)

type memoryKata struct {
	issue             kataclient.Issue
	created, comments int
	commentFails      bool
	conflictOnce      bool
	concurrentEntry   jsontext.Value
	markers           map[string]bool
}

func (*memoryKata) CheckProject(ref string) error {
	if project, _, qualified := strings.Cut(ref, "#"); qualified && project != "docbank" {
		return &kataclient.Error{Code: "wrong_project"}
	}
	return nil
}

func (k *memoryKata) snapshot() kataclient.Issue {
	issue := k.issue
	issue.Metadata = make(map[string]jsontext.Value, len(k.issue.Metadata))
	for key, value := range k.issue.Metadata {
		issue.Metadata[key] = value.Clone()
	}
	issue.Comments = slices.Clone(k.issue.Comments)
	return issue
}

func (k *memoryKata) Get(context.Context, string) (kataclient.Issue, error) { return k.snapshot(), nil }
func (k *memoryKata) FindMarker(_ context.Context, key string) (*kataclient.Issue, error) {
	if k.markers[key] {
		issue := k.snapshot()
		return &issue, nil
	}
	return nil, nil //nolint:nilnil // A missing marker is a successful lookup.
}
func (k *memoryKata) Create(_ context.Context, key, title, body string, metadata map[string]jsontext.Value) (kataclient.Issue, error) {
	k.created++
	k.markers = map[string]bool{key: true}
	k.issue = kataclient.Issue{UID: "synthetic-uid", ShortID: "abcd", QualifiedRef: "docbank#abcd", Title: title, Body: body, Metadata: metadata}
	return k.snapshot(), nil
}
func (k *memoryKata) Patch(_ context.Context, _, key string, previous, value jsontext.Value) (kataclient.Issue, error) {
	if k.conflictOnce {
		k.conflictOnce = false
		env, err := parseEnvelope(k.issue.Metadata[key])
		if err != nil {
			return kataclient.Issue{}, err
		}
		env.entries = append(env.entries, k.concurrentEntry.Clone())
		data, err := env.encode()
		if err != nil {
			return kataclient.Issue{}, err
		}
		k.issue.Metadata[key] = data
		k.issue.Metadata["unrelated"] = jsontext.Value(`"preserved"`)
		return kataclient.Issue{}, &kataclient.Error{Status: 409, Code: "metadata_guard_failed"}
	}
	if string(previous) != string(k.issue.Metadata[key]) {
		return kataclient.Issue{}, errors.New("wrong guard")
	}
	k.issue.Metadata[key] = value.Clone()
	return k.snapshot(), nil
}
func (k *memoryKata) Comment(_ context.Context, _, _, body string) error {
	if k.commentFails {
		return errors.New("synthetic outage")
	}
	k.comments++
	k.issue.Comments = append(k.issue.Comments, kataclient.Comment{Body: body})
	return nil
}

type memoryArchive struct {
	state      string
	reads      int
	prepareErr error
	evidence   *Evidence
}

func (*memoryArchive) VaultUID() string { return "11111111-1111-4111-8111-111111111111" }

func (a *memoryArchive) Prepare(context.Context, Selector) (Evidence, error) {
	if a.prepareErr != nil {
		return Evidence{}, a.prepareErr
	}
	if a.evidence != nil {
		return *a.evidence, nil
	}
	return NewEvidence(nativeReference(), "hello", "notes.txt"), nil
}

func TestCreateReplaySurvivesUnavailableSource(t *testing.T) {
	k := &memoryKata{}
	a := &memoryArchive{state: "available"}
	s := Service{Kata: k, Archive: a}
	r := CiteRequest{Selector: Selector{NodeID: 2, EndRune: 5}, Title: "Follow up", IdempotencyKey: "create-1"}
	_, err := s.Cite(t.Context(), r)
	require.NoError(t, err)
	a.prepareErr = ErrUnavailable
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 1, k.created)
}
func (a *memoryArchive) Resolve(_ context.Context, ref Reference) (Resolution, error) {
	a.reads++
	return Resolution{State: a.state, Evidence: NewEvidence(ref, "hello", "notes.txt")}, nil
}

func TestCiteCreateAndReplay(t *testing.T) {
	k := &memoryKata{}
	s := Service{Kata: k, Archive: &memoryArchive{state: "available"}}
	r := CiteRequest{Selector: Selector{NodeID: 2, EndRune: 5}, Title: "Follow up", Brief: "Review this.", IdempotencyKey: "create-1"}
	issue, err := s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, "Review this.\n\nEvidence: notes.txt\n\n> hello\n", issue.Body)
	var envelope struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(issue.Metadata[MetadataKey], &envelope))
	require.Equal(t, 1, envelope.Version)
	require.Len(t, envelope.Entries, 1)
	require.Equal(t, "docbank", envelope.Entries[0].Reference.Kind)
	require.Nil(t, envelope.Entries[0].Pending)
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 1, k.created)
	r.Title = "Different request"
	_, err = s.Cite(t.Context(), r)
	require.ErrorContains(t, err, "idempotency")
}

func TestAppendPendingRetryPreservesOtherEvidence(t *testing.T) {
	foreign := jsontext.Value(`{"version":1,"entries":[{"id":"old","passage":"old","reference":{"kind":"future","version":9},"future":"keep"}],"future":"keep"}`)
	k := &memoryKata{issue: kataclient.Issue{UID: "synthetic-uid", Metadata: map[string]jsontext.Value{MetadataKey: foreign, MsgvaultMetadataKey: jsontext.Value(`{"foreign":true}`)}}, commentFails: true, conflictOnce: true}
	concurrent := nativeReference()
	concurrent.Docbank.NodeID = 3
	var err error
	k.concurrentEntry, err = json.Marshal(Entry{ID: ID(concurrent), Passage: PassageID(concurrent, "other"), Reference: concurrent})
	require.NoError(t, err)
	s := Service{Kata: k, Archive: &memoryArchive{state: "available"}}
	r := CiteRequest{Issue: "abcd", Selector: Selector{NodeID: 2, EndRune: 5}}
	_, err = s.Cite(t.Context(), r)
	require.ErrorContains(t, err, "synthetic outage")
	require.Contains(t, string(k.issue.Metadata[MetadataKey]), `"pending"`)
	k.commentFails = false
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 1, k.comments)
	require.NotContains(t, string(k.issue.Metadata[MetadataKey]), `"pending"`)
	require.Contains(t, string(k.issue.Metadata[MetadataKey]), `"future":"keep"`)
	require.JSONEq(t, `{"foreign":true}`, string(k.issue.Metadata[MsgvaultMetadataKey]))
	require.Equal(t, `"preserved"`, string(k.issue.Metadata["unrelated"]))
	var envelope struct {
		Entries []Entry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(k.issue.Metadata[MetadataKey], &envelope))
	require.Len(t, envelope.Entries, 3)
	require.EqualValues(t, 3, envelope.Entries[1].Reference.Docbank.NodeID, "preserve the concurrent citation")
	require.EqualValues(t, 2, envelope.Entries[2].Reference.Docbank.NodeID, "append the requested citation")
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 1, k.comments)
}

func TestAppendRetryFinishesPendingAfterSourceDisappears(t *testing.T) {
	k := &memoryKata{issue: kataclient.Issue{UID: "synthetic-uid", Metadata: map[string]jsontext.Value{}}, commentFails: true}
	a := &memoryArchive{state: "available"}
	s := Service{Kata: k, Archive: a}
	r := CiteRequest{Issue: "abcd", Selector: Selector{NodeID: 2, EndRune: 5, ExpectedVersion: nativeReference().Docbank.ContentVersionID}}
	_, err := s.Cite(t.Context(), r)
	require.Error(t, err)
	k.commentFails = false
	a.prepareErr = ErrUnavailable
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 1, k.comments)
}

func TestCitationOnlyAppendRetryFinishesPendingAfterSourceDisappears(t *testing.T) {
	ref := nativeReference()
	ref.Docbank.RenditionAttachmentID = strings.Repeat("b", 64)
	ref.Docbank.BuildID = strings.Repeat("c", 64)
	ref.Docbank.RenditionSHA256 = strings.Repeat("d", 64)
	citation, err := ref.Docbank.TextCitation()
	require.NoError(t, err)
	evidence := NewEvidence(ref, "hello", "notes.txt")
	k := &memoryKata{issue: kataclient.Issue{UID: "synthetic-uid", Metadata: map[string]jsontext.Value{}}, commentFails: true}
	a := &memoryArchive{evidence: &evidence}
	s := Service{Kata: k, Archive: a}
	r := CiteRequest{Issue: "abcd", Selector: Selector{NodeID: 2, EndRune: 5, Citation: &citation}}
	_, err = s.Cite(t.Context(), r)
	require.ErrorContains(t, err, "synthetic outage")
	k.commentFails = false
	a.prepareErr = ErrUnavailable
	for _, field := range []string{"vault", "version"} {
		altered := citation
		if field == "vault" {
			altered.VaultUID[0]++
		} else {
			altered.ContentVersionID[0]++
		}
		conflicting := r
		conflicting.Selector.ExpectedVersion = ref.Docbank.ContentVersionID
		conflicting.Selector.Citation = &altered
		_, err = s.Cite(t.Context(), conflicting)
		require.ErrorIs(t, err, ErrUnavailable, "different citation %s must not finish the pending comment", field)
		require.Zero(t, k.comments)
	}
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 1, k.comments)
	require.NotContains(t, string(k.issue.Metadata[MetadataKey]), `"pending"`)
}

func TestContextFreshChangedUnavailableAndUnreachable(t *testing.T) {
	k := &memoryKata{}
	a := &memoryArchive{state: "available"}
	s := Service{Kata: k, Archive: a}
	_, err := s.Cite(t.Context(), CiteRequest{Title: "Follow up", IdempotencyKey: "create-1", Selector: Selector{NodeID: 2, EndRune: 5}})
	require.NoError(t, err)
	for _, state := range []string{"available", "changed", "unavailable", "unreachable"} {
		a.state = state
		page, err := s.Context(t.Context(), ContextRequest{Issue: "abcd"})
		require.NoError(t, err)
		require.Len(t, page.Passages, 1)
		require.Equal(t, state, page.Passages[0].State)
		require.Equal(t, "hello", page.Passages[0].SavedQuote)
	}
	s.Archive = nil
	page, err := s.Context(t.Context(), ContextRequest{Issue: "abcd"})
	require.NoError(t, err)
	require.Equal(t, "unreachable", page.Passages[0].State)
	require.Equal(t, "hello", page.Passages[0].SavedQuote)
}

func TestContextMsgvaultLimitsAndSavedQuote(t *testing.T) {
	ref := nativeReference()
	ref.Docbank.RenditionAttachmentID = ref.Docbank.ContentSHA256
	ref.Docbank.BuildID = ref.Docbank.ContentSHA256
	ref.Docbank.RenditionSHA256 = ref.Docbank.ContentSHA256
	legacy := map[string]any{"version": 1, "kind": "docbank_rendition", "archive_uid": "synthetic-archive", "message_id": int64(1), "source_type": "synthetic", "source_identifier": "synthetic-source", "source_message_id": "synthetic-message", "attachment_id": int64(1), "occurrence_key": "synthetic-occurrence", "docbank_rendition": ref.Docbank}
	// Fixed v1 fixture, independently encoded from msgvault reference.go fields.
	const legacyID = "7902404ab8f64c659f1ef1b785984a5f5ba90429f60ac3852e808aba419def30"
	const passage = "0e2e44473c8cfb767eaf3e847eb4c2b57ab48e3a45c08f595653257cf5dbdcd3"
	entries := []any{}
	for range 65 {
		entries = append(entries, map[string]any{"reference": map[string]any{"kind": "message"}})
	}
	entries = append(entries, map[string]any{"id": legacyID, "passage": passage, "reference": legacy})
	data, err := json.Marshal(map[string]any{"version": 1, "entries": entries})
	require.NoError(t, err)
	k := &memoryKata{issue: kataclient.Issue{Body: "Evidence: notes.txt\n\n> hello\n", Metadata: map[string]jsontext.Value{MsgvaultMetadataKey: data}}}
	s := Service{Kata: k}
	page, err := s.Context(t.Context(), ContextRequest{Issue: "abcd"})
	require.NoError(t, err)
	require.Len(t, page.Passages, 1)
	require.Equal(t, "hello", page.Passages[0].SavedQuote)
	require.Equal(t, "b0421bfba268295362c896049bc30d08474e6fa7ee017af70199767025777562", page.Passages[0].Evidence.ID, "normalized reference must carry its own identity")
	require.Equal(t, "a4b5f35889255faf592b2929f6876fe7198653092e4a7403a38ce7d17938bc3f", page.Passages[0].Evidence.Passage)
	legacy["version"] = 2
	entries[65] = map[string]any{"id": legacyID, "passage": passage, "reference": legacy}
	data, err = json.Marshal(map[string]any{"version": 1, "entries": entries})
	require.NoError(t, err)
	k.issue.Metadata[MsgvaultMetadataKey] = data
	s.Archive = &memoryArchive{state: "available"}
	page, err = s.Context(t.Context(), ContextRequest{Issue: "abcd"})
	require.NoError(t, err)
	require.Equal(t, "unsupported", page.Passages[0].State)
}

func TestAppendRefusesEnvelopeThatWouldExceedReadLimit(t *testing.T) {
	previous, err := json.Marshal(map[string]any{"version": 1, "entries": []any{}, "future": strings.Repeat("x", (512<<10)-200)})
	require.NoError(t, err)
	k := &memoryKata{issue: kataclient.Issue{UID: "synthetic-uid", Metadata: map[string]jsontext.Value{MetadataKey: previous}}}
	s := Service{Kata: k, Archive: &memoryArchive{state: "available"}}
	_, err = s.Cite(t.Context(), CiteRequest{Issue: "abcd", Selector: Selector{NodeID: 2, EndRune: 5}})
	require.Error(t, err)
	require.Equal(t, jsontext.Value(previous), k.issue.Metadata[MetadataKey], "refuse oversized metadata before changing Kata")
	require.Zero(t, k.comments)
}

func TestAppendRejectsFullEnvelopeAsPermanentUnsupported(t *testing.T) {
	entries := make([]jsontext.Value, MaxEntries)
	for i := range entries {
		entries[i] = jsontext.Value(`{"reference":{"kind":"future"}}`)
	}
	previous, err := json.Marshal(map[string]any{"version": 1, "entries": entries})
	require.NoError(t, err)
	k := &memoryKata{issue: kataclient.Issue{UID: "synthetic-uid", Metadata: map[string]jsontext.Value{MetadataKey: previous}}}
	s := Service{Kata: k, Archive: &memoryArchive{state: "available"}}

	_, err = s.Cite(t.Context(), CiteRequest{Issue: "abcd", Selector: Selector{NodeID: 2, EndRune: 5}})

	require.ErrorIs(t, err, ErrUnsupported)
	require.ErrorContains(t, err, "already has 64 Docbank evidence entries")
	require.NotContains(t, strings.ToLower(err.Error()), "retry")
	require.Equal(t, jsontext.Value(previous), k.issue.Metadata[MetadataKey])
	require.Zero(t, k.comments)
}

func TestContextUnsupportedNativeDoesNotHideOtherPassages(t *testing.T) {
	future := nativeReference()
	future.Version = 2
	entries := []Entry{
		{ID: ID(future), Passage: PassageID(future, "hello"), Reference: future},
		{ID: ID(nativeReference()), Passage: PassageID(nativeReference(), "hello"), Reference: nativeReference()},
	}
	data, err := json.Marshal(map[string]any{"version": 1, "entries": entries})
	require.NoError(t, err)
	k := &memoryKata{issue: kataclient.Issue{Body: "Evidence: notes.txt\n\n> hello\n", Metadata: map[string]jsontext.Value{MetadataKey: data}}}
	a := &memoryArchive{state: "available"}
	page, err := (Service{Kata: k, Archive: a}).Context(t.Context(), ContextRequest{Issue: "abcd"})
	require.NoError(t, err)
	require.Len(t, page.Passages, 2)
	require.Equal(t, "unsupported", page.Passages[0].State)
	require.Equal(t, "hello", page.Passages[0].SavedQuote)
	require.Equal(t, "available", page.Passages[1].State)
	require.Equal(t, 1, a.reads)
}

func TestContextMalformedReferenceDoesNotHideOtherPassages(t *testing.T) {
	for _, malformed := range []string{
		`{"reference":{"kind":"docbank","docbank":[]}}`,
		`{"reference":{"kind":"docbank_rendition","docbank_rendition":[]}}`,
		`{"reference":[]}`,
	} {
		valid := Entry{ID: ID(nativeReference()), Passage: PassageID(nativeReference(), "hello"), Reference: nativeReference()}
		encoded, err := json.Marshal(valid)
		require.NoError(t, err)
		data, err := json.Marshal(map[string]any{"version": 1, "entries": []jsontext.Value{jsontext.Value(malformed), encoded}})
		require.NoError(t, err)
		k := &memoryKata{issue: kataclient.Issue{Body: "Evidence: notes.txt\n\n> hello\n", Metadata: map[string]jsontext.Value{MetadataKey: data}}}
		a := &memoryArchive{state: "available"}
		page, err := (Service{Kata: k, Archive: a}).Context(t.Context(), ContextRequest{Issue: "abcd"})
		require.NoError(t, err)
		require.Len(t, page.Passages, 2)
		require.Equal(t, "unsupported", page.Passages[0].State)
		require.Empty(t, page.Passages[0].SavedQuote, "malformed references cannot claim another entry's quote")
		require.Equal(t, "available", page.Passages[1].State)
		require.Equal(t, "hello", page.Passages[1].SavedQuote)
		require.Equal(t, 1, a.reads)
	}
}

func TestCiteRejectsInvalidRequests(t *testing.T) {
	for _, request := range []CiteRequest{
		{Selector: Selector{NodeID: 2, EndRune: 1001}, Title: "Too much", IdempotencyKey: "key"},
		{Selector: Selector{NodeID: 2, StartRune: 5, EndRune: 3}, Title: "Backward", IdempotencyKey: "key"},
		{Selector: Selector{NodeID: 2, EndRune: 5}, Title: "Missing key"},
		{Selector: Selector{NodeID: 2, EndRune: 5}, Issue: "abcd", Title: "Append title"},
	} {
		_, err := (Service{}).Cite(t.Context(), request)
		require.ErrorIs(t, err, ErrInvalid)
	}
}
