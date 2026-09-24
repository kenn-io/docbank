package production

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
)

func TestBuildArtifactProvenancePinsOccurrencesAcrossVolumes(t *testing.T) {
	job, _, members := packageProjectionFixture(t)
	firstVersion := "55555555-5555-4555-8555-555555555551"
	for index := range job.Manifest.Artifacts {
		artifact := &job.Manifest.Artifacts[index]
		if artifact.MemberID == members[0].ID {
			artifact.Path = fmt.Sprintf("VOL001/custom-name/%s/duplicate.txt", artifact.ID)
			artifact.Volume = "VOL001"
		} else {
			artifact.Path = fmt.Sprintf("VOL002/other-name/%s/duplicate.txt", artifact.ID)
			artifact.Volume = "VOL002"
		}
	}
	resealPackageJob(t, &job)
	sources := []RetainSource{
		{MemberID: members[0].ID, MemberOrdinal: 1, SourceVersionID: firstVersion},
		{MemberID: members[1].ID, MemberOrdinal: 2, SourceVersionID: firstVersion},
	}
	receipt, err := BuildArtifactProvenance(job, sources)
	require.NoError(t, err)
	require.NoError(t, documentproduction.ValidateArtifactProvenanceReceipt(receipt))
	require.Equal(t, job.Manifest.SHA256, receipt.ArtifactManifestSHA256)
	require.Len(t, receipt.Entries, len(job.Manifest.Artifacts))
	for _, entry := range receipt.Entries {
		require.Equal(t, firstVersion, entry.SourceVersionID)
		artifact := artifactByID(t, job.Manifest, entry.ArtifactID)
		require.Equal(t, artifact.SHA256, entry.ArtifactSHA256)
		require.Equal(t, artifact.MemberID, entry.MemberID)
		require.Equal(t, artifact.MemberOrdinal, entry.MemberOrdinal)
		require.Equal(t, artifact.Page, entry.Page)
		require.Equal(t, artifact.Volume, entry.Volume)
	}
	repeated, err := BuildArtifactProvenance(job, sources)
	require.NoError(t, err)
	require.Equal(t, receipt, repeated)
}

func TestBuildArtifactProvenanceRejectsUnboundAndChangedIdentity(t *testing.T) {
	job, _, members := packageProjectionFixture(t)
	sources := []RetainSource{{MemberID: members[0].ID, MemberOrdinal: 1,
		SourceVersionID: "55555555-5555-4555-8555-555555555551"}}
	_, err := BuildArtifactProvenance(job, sources)
	require.Error(t, err, "a missing occurrence must not borrow another source version")
	sources = append(sources, RetainSource{MemberID: members[1].ID, MemberOrdinal: 2,
		SourceVersionID: "55555555-5555-4555-8555-555555555552"})
	job.Manifest.Artifacts[0].SHA256 = testHash("tampered")
	_, err = BuildArtifactProvenance(job, sources)
	require.Error(t, err, "an artifact changed without republishing its manifest must fail")
}

func artifactByID(t *testing.T, manifest documentproduction.ArtifactManifest, id string) documentproduction.Artifact {
	t.Helper()
	for _, artifact := range manifest.Artifacts {
		if artifact.ID == id {
			return artifact
		}
	}
	t.Fatalf("artifact %s not found", id)
	return documentproduction.Artifact{}
}
