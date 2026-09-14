package store

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenditionAttachmentIDCompatibility(t *testing.T) {
	for _, tc := range []struct{ publication, version, profile, want string }{
		{"job-seed", "version-a", "profile-a", "b114d1f024daf5b4401c09ca935598e1eb43b045423e9ff285f1465306b336cb"},
		{"body-build", "version-a", "profile-a", "aab2b841a93445b91f7855368194abf5e09985fa9cc162c2bc7974fdf13fd760"},
		{"body-build", "version-b", "profile-a", "86d4ea16593dac4b317be7324042d9a94d44c64122b4baec211f17f9c25b91f7"},
		{"body-build", "version-a", "profile-b", "ff59f6f1bdcb00933f57adbfcdf97addb67f1097ba4d4cfbe9baa764d1898dcb"},
	} {
		require.Equal(t, tc.want, RenditionAttachmentID(tc.publication, tc.version, tc.profile))
	}
}
func TestRenditionAttachmentIDQueuedJobSeed(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	request := renditionJobTestRequest(versions[0], profile)
	grantRenditionJobConsent(t, s, request)
	job, waiter, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	digest := sha256.Sum256([]byte("docbank:rendition-attachment:v1\x00" + job.ID + "\x00" + versions[0] + "\x00" + profile.Fingerprint))
	require.Equal(t, hex.EncodeToString(digest[:]), waiter.AttachmentID)
	require.Equal(t, waiter.AttachmentID, RenditionAttachmentID(job.ID, versions[0], profile.Fingerprint))
}
