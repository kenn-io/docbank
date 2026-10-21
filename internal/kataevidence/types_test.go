package kataevidence

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func nativeReference() Reference {
	return Reference{Version: 1, Kind: "docbank", Docbank: &DocumentReference{
		VaultUID: "11111111-1111-4111-8111-111111111111", NodeID: 2,
		ContentVersionID: "22222222-2222-4222-8222-222222222222",
		ContentSHA256:    strings.Repeat("a", 64), StartRune: 0, EndRune: 5,
	}}
}

func TestReferenceWireAndIdentity(t *testing.T) {
	ref := nativeReference()
	require.NoError(t, ValidateReference(ref))
	encoded, err := json.Marshal(ref)
	require.NoError(t, err)
	require.JSONEq(t, `{"version":1,"kind":"docbank","docbank":{"vault_uid":"11111111-1111-4111-8111-111111111111","node_id":2,"content_version_id":"22222222-2222-4222-8222-222222222222","content_sha256":"`+strings.Repeat("a", 64)+`","start_rune":0,"end_rune":5}}`, string(encoded))
	// Fixed length-prefixed SHA-256 fixtures protect persisted v1 identities.
	require.Equal(t, "f53f934a8ef26d2ef3678198fb3d8cd0af94ce1c640a386fa1a2b691261e2f6b", ID(ref))
	before := ID(ref)
	ref.Docbank.NodeID++
	require.NotEqual(t, before, ID(ref))
	ref.Docbank.NodeID--
	ref.Docbank.ContentSHA256 = strings.Repeat("b", 64)
	require.NotEqual(t, before, ID(ref))
	ref.Docbank.EndRune = 1001
	require.Error(t, ValidateReference(ref))
}

func TestReplayDigestGoldens(t *testing.T) {
	r := CiteRequest{Selector: Selector{NodeID: 2, EndRune: 5, ExpectedVersion: "22222222-2222-4222-8222-222222222222", Quote: "hello"}, Title: "Follow up", Brief: "Review this.", IdempotencyKey: "create-1"}
	require.Equal(t, "8690c8c09dd46f5b993d07a35df65e63663fa3ef8ad54228f36fa243b4de0283", requestDigest(r))
	require.Equal(t, "af373bbaadf75db7f653a67f03a7b9d6fe16d047571cf39b76a95d99ca466e73", createMarker("11111111-1111-4111-8111-111111111111", "create-1"))
	require.Equal(t, "2933953e9c059ecaf7396ab0eddab4a0b2eefa3a261b9ff0be76e9739d29e6bf", commentKey("synthetic-uid", "f53f934a8ef26d2ef3678198fb3d8cd0af94ce1c640a386fa1a2b691261e2f6b", "Evidence: notes.txt\n\n> hello\n\n<!-- docbank-evidence:f53f934a8ef26d2ef3678198fb3d8cd0af94ce1c640a386fa1a2b691261e2f6b -->\n"))
	r.Selector.NodeID, r.Selector.Path = 0, "/notes.txt"
	require.Equal(t, "58c0b31dc1809463cd2eea236fc693dee9e9b241a48a48bfaae912e62a9b169f", requestDigest(r))
	citation := renditionReference().Rendition
	r.Selector.NodeID, r.Selector.Path, r.Selector.Citation = 2, "", citation
	require.Equal(t, "c9b656a395e298ff50d3e7b5ef04135cbd50f537f76e4e77c4404eb434fba3e4", requestDigest(r))
}

func renditionReference() Reference {
	return Reference{Version: 1, Kind: "docbank", Rendition: &document.TextCitation{
		Version: 1, VaultUID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), NodeID: 2,
		ContentVersionID: uuid.MustParse("22222222-2222-4222-8222-222222222222"), ContentSHA256: strings.Repeat("a", 64),
		RenditionAttachmentID: strings.Repeat("b", 64), BuildID: strings.Repeat("c", 64), RenditionSHA256: strings.Repeat("d", 64), Start: 0, End: 5,
	}}
}

func TestRenditionReferenceWireAndIdentity(t *testing.T) {
	ref := renditionReference()
	require.NoError(t, ValidateReference(ref))
	encoded, err := json.Marshal(ref)
	require.NoError(t, err)
	require.JSONEq(t, `{"version":1,"kind":"docbank","rendition":{"version":1,"vault_uid":"11111111-1111-4111-8111-111111111111","node_id":2,"content_version_id":"22222222-2222-4222-8222-222222222222","content_sha256":"`+strings.Repeat("a", 64)+`","rendition_attachment_id":"`+strings.Repeat("b", 64)+`","build_id":"`+strings.Repeat("c", 64)+`","rendition_sha256":"`+strings.Repeat("d", 64)+`","start":0,"end":5}}`, string(encoded))
	require.Equal(t, "7eb2ec347eee5010686228adae444edf6517d881b3b868237c6b2dc0a1a6be35", ID(ref))
	ref.Docbank = nativeReference().Docbank
	require.ErrorIs(t, ValidateReference(ref), ErrInvalid, "ambiguous sources must fail")
	ref.Docbank = nil
	ref.Rendition.BuildID = ""
	require.ErrorIs(t, ValidateReference(ref), ErrInvalid)
	ref.Rendition.BuildID = strings.Repeat("c", 64)
	ref.Rendition.End = 1001
	require.ErrorIs(t, ValidateReference(ref), ErrInvalid)
}
