package document

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPeopleIdentityCollisionBoundaries(t *testing.T) {
	upper, err := NormalizePersonIdentity("email", "Ada <Ada@EXAMPLE.TEST>")
	require.NoError(t, err)
	lower, err := NormalizePersonIdentity("email", "ada@example.test")
	require.NoError(t, err)
	require.Equal(t, "Ada@example.test", upper.ValueNormalized)
	require.NotEqual(t, upper.ValueNormalized, lower.ValueNormalized)
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
}
