package kataevidence

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
	require.Equal(t, "138fdacd9800c859e71486d8f2566c79892b984f827ac951bc1b27468d7706d8", PassageID(ref, "hello"))
	before := ID(ref)
	ref.Docbank.NodeID++
	require.NotEqual(t, before, ID(ref))
	ref.Docbank.NodeID--
	ref.Docbank.ContentSHA256 = strings.Repeat("b", 64)
	require.NotEqual(t, before, ID(ref))
	ref.Docbank.BuildID = strings.Repeat("c", 64)
	require.Error(t, ValidateReference(ref), "partial rendition identity must fail")
	ref.Docbank.BuildID = ""
	ref.Docbank.EndRune = 1001
	require.Error(t, ValidateReference(ref))
}

func TestReplayDigestGoldens(t *testing.T) {
	r := CiteRequest{Selector: Selector{NodeID: 2, EndRune: 5, ExpectedVersion: "22222222-2222-4222-8222-222222222222", Quote: "hello"}, Title: "Follow up", Brief: "Review this.", IdempotencyKey: "create-1"}
	require.Equal(t, "8690c8c09dd46f5b993d07a35df65e63663fa3ef8ad54228f36fa243b4de0283", requestDigest(r))
	require.Equal(t, "af373bbaadf75db7f653a67f03a7b9d6fe16d047571cf39b76a95d99ca466e73", createMarker("11111111-1111-4111-8111-111111111111", "create-1"))
	require.Equal(t, "4e63d936039ece9d6a0b1e09c9c34bbc1a770a1809894a324f8fba100a5bef98", commentKey("synthetic-uid", "138fdacd9800c859e71486d8f2566c79892b984f827ac951bc1b27468d7706d8", renderQuote("notes.txt", "hello")))
	r.Selector.NodeID, r.Selector.Path = 0, "/notes.txt"
	require.Equal(t, "58c0b31dc1809463cd2eea236fc693dee9e9b241a48a48bfaae912e62a9b169f", requestDigest(r))
	ref := nativeReference()
	ref.Docbank.RenditionAttachmentID, ref.Docbank.BuildID, ref.Docbank.RenditionSHA256 = strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	citation, err := ref.Docbank.TextCitation()
	require.NoError(t, err)
	r.Selector.NodeID, r.Selector.Path, r.Selector.Citation = 2, "", &citation
	require.Equal(t, "c9b656a395e298ff50d3e7b5ef04135cbd50f537f76e4e77c4404eb434fba3e4", requestDigest(r))
}
