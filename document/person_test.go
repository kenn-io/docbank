package document

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPeopleIdentityCollisionBoundaries(t *testing.T) {
	upper, err := NormalizePersonIdentity("email", "Ada <Ada@EXAMPLE.TEST>")
	require.NoError(t, err)
	lower, err := NormalizePersonIdentity("email", "ada@example.test")
	require.NoError(t, err)
	require.Equal(t, "ada@example.test", upper.ValueNormalized)
	require.Equal(t, upper.ValueNormalized, lower.ValueNormalized)
	national, err := NormalizePersonIdentity("phone", "555-0100")
	require.NoError(t, err)
	require.False(t, national.AutoLinkEligible)
	international, err := NormalizePersonIdentity("phone", "+1 (555) 0100")
	require.NoError(t, err)
	require.True(t, international.AutoLinkEligible)
	left, err := ExternalPersonActorKey("msgvault", "a/b", "c")
	require.NoError(t, err)
	right, err := ExternalPersonActorKey("msgvault", "a", "b/c")
	require.NoError(t, err)
	require.NotEqual(t, left, right)
	require.LessOrEqual(t, len(left), MaxActorKeyBytes)
	require.Equal(t, "fi ada", FoldPersonName("ﬁ   ADA"))
	_, err = NormalizePersonIdentity("email", "a@example.test\r\nBcc:b@example.test")
	require.Error(t, err)
	_, err = NormalizePersonIdentity("phone", "+0123456")
	require.Error(t, err)
	_, err = NormalizePersonIdentity("phone", strings.Repeat("1", 16))
	require.Error(t, err)
}

func TestPersonEmailActorKeyMatchesV1QuotedLocalPart(t *testing.T) {
	for _, test := range []struct{ raw, key string }{
		{`"<Ada"@example.test`, `email:ada@example.test`},
		{`" Ada"@example.test`, `email:ada@example.test`},
	} {
		t.Run(test.raw, func(t *testing.T) {
			identity, err := NormalizePersonIdentity("email", test.raw)
			require.NoError(t, err)
			key, err := ActorKey(identity)
			require.NoError(t, err)
			require.Equal(t, test.key, key)
			require.Equal(t, test.raw, identity.ValueDisplay)
		})
	}
	_, err := NormalizePersonIdentity("email", `"< Ada"@example.test`)
	require.Error(t, err, "reject a key that V1 decoding would normalize again")
}

func TestPersonHandleActorKeyMatchesV1(t *testing.T) {
	const raw = "ÄPP/user-a"
	identity, err := NormalizePersonIdentity("handle", raw)
	require.NoError(t, err)
	personKey, err := ActorKey(identity)
	require.NoError(t, err)
	eventKey, err := ActorKeyV1("handle", raw)
	require.NoError(t, err)
	require.Equal(t, eventKey, personKey)
}
