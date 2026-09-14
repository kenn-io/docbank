package transfer

import (
	"strings"
	"testing"

	"encoding/json/jsontext"

	"github.com/stretchr/testify/require"
)

func TestReservedContainersCannotEvadeDottedPrefixRule(t *testing.T) {
	for _, key := range []string{"token", "Token.access", "sync", "Refresh_Token", "credential.value", "private_notes"} {
		require.True(t, ForbiddenObjectKey(key), key)
	}
	require.False(t, ForbiddenObjectKey("body_text"))
	require.False(t, ForbiddenObjectKey("record_ref"))
}

func TestForbiddenObjectKeyListIsPinned(t *testing.T) {
	require.Equal(t, []string{
		"credential", "token", "secret", "oauth", "cursor", "sync", "carddav",
		"imap", "session", "cookie", "enrichment",
	}, forbiddenObjectPrefixes)
	require.Equal(t, []string{
		"password", "passphrase", "api_key", "apikey", "access_token", "refresh_token",
		"client_secret", "uidvalidity", "provider_credential", "private_notes",
	}, forbiddenObjectNames)
}

func TestStructuredKeyScanRejectsNestedSecretsWithoutParsingStrings(t *testing.T) {
	err := CheckStructuredKeys(jsontext.Value(`{"source_fields":{"safe":{"OAuth.client":"private-value"}}}`))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "OAuth.client")
	require.NotContains(t, err.Error(), "private-value")

	require.NoError(t, CheckStructuredKeys(jsontext.Value(`{"body_text":"{\"token\":\"ordinary message text\"}"}`)))
}

func TestStructuredKeyScanEnforcesDepthBound(t *testing.T) {
	raw := strings.Repeat(`{"safe":`, MaxJSONDepth) + `{"safe":true}` + strings.Repeat(`}`, MaxJSONDepth)
	err := CheckStructuredKeys(jsontext.Value(raw))
	require.Error(t, err)
	require.ErrorIs(t, err, errStructuredDepth)
}
