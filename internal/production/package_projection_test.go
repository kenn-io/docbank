package production

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
)

const (
	packageJobID = "11111111-1111-4111-8111-111111111111"
	packageSetID = "22222222-2222-4222-8222-222222222222"
	packageOneID = "33333333-3333-4333-8333-333333333333"
	packageTwoID = "44444444-4444-4444-8444-444444444444"
)

func packageProjectionFixture(t *testing.T) (Job, documentproduction.NumberReservation, []PackageMember) {
	t.Helper()
	members := []PackageMember{{ID: packageOneID, Ordinal: 1, FamilyID: "family-one"},
		{ID: packageTwoID, Ordinal: 2, FamilyID: "family-one"}}
	numbers := documentproduction.NumberReservation{
		Contract: documentproduction.NumberReservationContractV1, Authority: "synthetic-ledger",
		ID: "55555555-5555-4555-8555-555555555555", OperationID: packageJobID,
		RevisionSHA256: testHash("revision"), State: "reserved",
		Numbers: []documentproduction.AssignedNumber{
			{MemberID: packageOneID, MemberOrdinal: 1, Page: 1, Text: "ÉX-0001"},
			{MemberID: packageOneID, MemberOrdinal: 1, Page: 2, Text: "ÉX-0002"},
			{MemberID: packageTwoID, MemberOrdinal: 2, Page: 1, Text: "ÉX-0003"},
		},
	}
	_, numbers.SHA256, _ = documentproduction.CanonicalNumberReservation(numbers)
	artifacts := []documentproduction.Artifact{}
	for index, member := range members {
		ordinal := int64(index + 1)
		for _, role := range []string{documentproduction.ArtifactRoleRedactedPDF, documentproduction.ArtifactRoleRedactedText} {
			media := "application/pdf"
			if role == documentproduction.ArtifactRoleRedactedText {
				media = "text/plain; charset=utf-8"
			}
			artifacts = append(artifacts, documentproduction.Artifact{
				ID: packageArtifactID(len(artifacts) + 1), MemberID: member.ID, MemberOrdinal: ordinal,
				Role: role, Path: "private/artifact-" + string(rune('a'+len(artifacts))),
				SHA256: testHash(role + member.ID), Size: 10, MediaType: media,
			})
		}
		pages := 1
		if index == 0 {
			pages = 2
		}
		for page := 1; page <= pages; page++ {
			artifacts = append(artifacts, documentproduction.Artifact{
				ID: packageArtifactID(len(artifacts) + 1), MemberID: member.ID, MemberOrdinal: ordinal,
				Page: page, Role: documentproduction.ArtifactRoleRedactedPage,
				Path:   "private/artifact-" + string(rune('a'+len(artifacts))),
				SHA256: testHash(member.ID + string(rune('0'+page))), Size: 10, MediaType: "image/png",
			})
		}
	}
	manifest := documentproduction.ArtifactManifest{Contract: documentproduction.ArtifactManifestContractV1, Artifacts: artifacts}
	_, manifest.SHA256, _ = documentproduction.CanonicalArtifactManifest(manifest)
	receipt := documentproduction.ProductionReceipt{
		Contract: documentproduction.ProductionReceiptContractV1, ID: packageJobID, JobID: packageJobID,
		SetID: packageSetID, Revision: 1, RevisionSHA256: numbers.RevisionSHA256,
		PreparedInputSHA256: testHash("prepared"), PolicySHA256: testHash("policy"),
		NumberReservationSHA256: numbers.SHA256, LayoutSHA256: testHash("layout"),
		EndorsementsSHA256: testHash("endorsements"), ArtifactManifestSHA256: manifest.SHA256,
		CreatedAt: "2026-09-23T12:00:00Z",
	}
	_, receipt.SHA256, _ = documentproduction.CanonicalProductionReceipt(receipt)
	return Job{ID: packageJobID, SetID: packageSetID, Revision: 1, RevisionSHA256: numbers.RevisionSHA256,
		PreparedInputSHA256: receipt.PreparedInputSHA256, State: ProductionJobSucceeded,
		Receipt: receipt, Manifest: manifest}, numbers, members
}

func packageArtifactID(number int) string {
	return fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", number)
}

func resealPackageJob(t *testing.T, job *Job) {
	t.Helper()
	_, digest, err := documentproduction.CanonicalArtifactManifest(job.Manifest)
	require.NoError(t, err)
	job.Manifest.SHA256 = digest
	job.Receipt.ArtifactManifestSHA256 = digest
	_, digest, err = documentproduction.CanonicalProductionReceipt(job.Receipt)
	require.NoError(t, err)
	job.Receipt.SHA256 = digest
}

func TestPlanPackageProjectionAllowsOnlyRedactedPublicFields(t *testing.T) {
	for _, profile := range []string{"export-dat-pdf-v1", "export-dat-opt-images-v1", "export-dat-lfp-images-v1"} {
		t.Run(profile, func(t *testing.T) {
			job, numbers, members := packageProjectionFixture(t)
			projection, err := PlanPackageProjection(job, numbers, members, profile, PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
			require.NoError(t, err)
			require.Equal(t, []string{"ÉX-0001", "ÉX-0002", "ÉX-0003"}, projection.PageNumbers())
			require.Len(t, projection.Manifest.Volumes, 1)
			require.Equal(t, "VOL001", projection.Manifest.Volumes[0].Name)
			require.Len(t, projection.Manifest.Documents, 2)
			require.Len(t, projection.Manifest.Documents[0].Images, 2)
			require.Equal(t, "IMAGES/DOC000001-000001.png", projection.Manifest.Documents[0].Images[0].Path)
			encoded, err := json.Marshal(projection.Manifest)
			require.NoError(t, err)
			for _, secret := range []string{packageOneID, packageTwoID, packageJobID, "private/", "source_sha256", "member_id", "reason"} {
				require.NotContains(t, string(encoded), secret)
			}
			if profile == "export-dat-pdf-v1" {
				require.NotEmpty(t, projection.Manifest.Documents[0].PDFPath)
				require.Equal(t, int64(70), projection.Manifest.Volumes[0].Bytes)
				require.Len(t, projection.bindings, 7)
			} else {
				require.Empty(t, projection.Manifest.Documents[0].PDFPath)
				require.Equal(t, int64(50), projection.Manifest.Volumes[0].Bytes)
				require.Len(t, projection.bindings, 5)
			}
			require.Equal(t, documentproduction.ArtifactRoleRedactedPage, projection.bindings[len(projection.bindings)-1].artifact.Role)
		})
	}
}

func TestPlanPackageProjectionRejectsUnsafeAndIncompleteRoles(t *testing.T) {
	for _, change := range []struct {
		name   string
		edit   func(*Job)
		reseal bool
	}{
		{"native", func(job *Job) { job.Manifest.Artifacts[0].Role = "native" }, false},
		{"generic", func(job *Job) { job.Manifest.Artifacts[0].Role = "original" }, false},
		{"valid nonpublic role", func(job *Job) {
			job.Manifest.Artifacts = append(job.Manifest.Artifacts, documentproduction.Artifact{
				ID: packageArtifactID(9), Role: documentproduction.ArtifactRoleDAT,
				Path: "private/extra", SHA256: testHash("dat"), Size: 10, MediaType: "text/plain",
			})
		}, true},
		{"missing text", func(job *Job) {
			job.Manifest.Artifacts = append(job.Manifest.Artifacts[:1], job.Manifest.Artifacts[2:]...)
		}, true},
		{"missing page", func(job *Job) {
			job.Manifest.Artifacts = append(job.Manifest.Artifacts[:2], job.Manifest.Artifacts[3:]...)
		}, true},
		{"duplicate page", func(job *Job) { job.Manifest.Artifacts[2].Page = 2 }, true},
		{"tampered hash", func(job *Job) { job.Manifest.Artifacts[0].SHA256 = testHash("changed") }, false},
	} {
		t.Run(change.name, func(t *testing.T) {
			job, numbers, members := packageProjectionFixture(t)
			change.edit(&job)
			if change.reseal {
				resealPackageJob(t, &job)
			}
			_, err := PlanPackageProjection(job, numbers, members, "export-dat-opt-images-v1", PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
			require.Error(t, err)
		})
	}
}

func TestPlanPackageProjectionIsIndependentOfInputSliceOrder(t *testing.T) {
	job, numbers, members := packageProjectionFixture(t)
	first, err := PlanPackageProjection(job, numbers, members, "export-dat-opt-images-v1", PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
	require.NoError(t, err)
	slices.Reverse(job.Manifest.Artifacts)
	slices.Reverse(numbers.Numbers)
	slices.Reverse(members)
	second, err := PlanPackageProjection(job, numbers, members, "export-dat-opt-images-v1", PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
	require.NoError(t, err)
	require.Equal(t, first.Manifest, second.Manifest)
}

func TestPlanPackageProjectionRejectsProfileAndVolumeBoundary(t *testing.T) {
	job, numbers, members := packageProjectionFixture(t)
	_, err := PlanPackageProjection(job, numbers, members, "export-csv-natives-v1", PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
	require.Error(t, err)
	_, err = PlanPackageProjection(job, numbers, members, "export-dat-opt-images-v1", PackageLimits{MaxVolumeBytes: 35, MaxVolumeDocuments: 10})
	require.Error(t, err, "one family cannot be split to fit a volume")
	members[1].FamilyID = "family-two"
	projection, err := PlanPackageProjection(job, numbers, members, "export-dat-opt-images-v1", PackageLimits{MaxVolumeBytes: 35, MaxVolumeDocuments: 10})
	require.NoError(t, err)
	require.Len(t, projection.Manifest.Volumes, 2)
	require.Equal(t, "VOL002", projection.Manifest.Documents[1].Volume)
}

func TestRecipientPackageVolumeLimitMatchesReceivedPackages(t *testing.T) {
	job, numbers, members := packageProjectionFixture(t)
	members[1].FamilyID = "family-two"
	seedArtifacts := slices.Clone(job.Manifest.Artifacts[4:])
	appendMember := func(ordinal int) {
		memberID := fmt.Sprintf("33333333-3333-4333-8333-%012d", ordinal)
		members = append(members, PackageMember{ID: memberID, Ordinal: int64(ordinal),
			FamilyID: fmt.Sprintf("family-%03d", ordinal)})
		numbers.Numbers = append(numbers.Numbers, documentproduction.AssignedNumber{
			MemberID: memberID, MemberOrdinal: int64(ordinal), Page: 1,
			Text: fmt.Sprintf("ÉX-%04d", ordinal+1)})
		for _, seed := range seedArtifacts {
			artifact := seed
			artifact.ID = packageArtifactID(len(job.Manifest.Artifacts) + 1)
			artifact.MemberID, artifact.MemberOrdinal = memberID, int64(ordinal)
			artifact.Path = fmt.Sprintf("private/volume-%03d-%s-%d", ordinal, artifact.Role, artifact.Page)
			artifact.SHA256 = testHash(artifact.Path)
			job.Manifest.Artifacts = append(job.Manifest.Artifacts, artifact)
		}
	}
	seal := func() {
		var err error
		_, numbers.SHA256, err = documentproduction.CanonicalNumberReservation(numbers)
		require.NoError(t, err)
		job.Receipt.NumberReservationSHA256 = numbers.SHA256
		resealPackageJob(t, &job)
	}
	for ordinal := 3; ordinal <= maxRecipientVolumes; ordinal++ {
		appendMember(ordinal)
	}
	seal()
	limits := PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 1}
	projection, err := PlanPackageProjection(job, numbers, members, "export-dat-pdf-v1", limits)
	require.NoError(t, err)
	require.Len(t, projection.Manifest.Volumes, maxRecipientVolumes)

	appendMember(maxRecipientVolumes + 1)
	seal()
	_, err = PlanPackageProjection(job, numbers, members, "export-dat-pdf-v1", limits)
	require.ErrorIs(t, err, ErrPackageProjection)

	manifest := projection.Manifest
	lastDoc := manifest.Documents[63]
	lastDoc.Pages = slices.Clone(lastDoc.Pages)
	lastDoc.Images = slices.Clone(lastDoc.Images)
	lastDoc.Volume = "VOL065"
	lastDoc.Control, lastDoc.End = "ÉX-0066", "ÉX-0066"
	lastDoc.Pages[0].Number = lastDoc.Control
	lastDoc.Images[0].Number = lastDoc.Control
	lastDoc.Images[0].Path = "IMAGES/DOC000065-000001.png"
	lastDoc.TextPath = "TEXT/DOC000065.txt"
	lastDoc.PDFPath = "PDF/DOC000065.pdf"
	manifest.Documents = append(manifest.Documents, lastDoc)
	lastVolume := manifest.Volumes[63]
	lastVolume.Name = "VOL065"
	manifest.Volumes = append(manifest.Volumes, lastVolume)
	_, err = packageExpectedPaths(manifest)
	require.ErrorIs(t, err, ErrRecipientArchive)
}

func TestPlanPackageProjectionRejectsChangedNumbersAndUnsafeLabels(t *testing.T) {
	for _, change := range []struct {
		name   string
		edit   func(*documentproduction.NumberReservation)
		reseal bool
	}{
		{"tampered receipt", func(value *documentproduction.NumberReservation) { value.Numbers[0].Text = "changed" }, false},
		{"control label", func(value *documentproduction.NumberReservation) { value.Numbers[0].Text = "EX\n0001" }, true},
		{"bidi label", func(value *documentproduction.NumberReservation) { value.Numbers[0].Text = "EX\u202e0001" }, true},
		{"page gap", func(value *documentproduction.NumberReservation) { value.Numbers[1].Page = 3 }, true},
	} {
		t.Run(change.name, func(t *testing.T) {
			job, numbers, members := packageProjectionFixture(t)
			change.edit(&numbers)
			if change.reseal {
				_, digest, err := documentproduction.CanonicalNumberReservation(numbers)
				require.NoError(t, err)
				numbers.SHA256 = digest
				job.Receipt.NumberReservationSHA256 = digest
				resealPackageJob(t, &job)
			}
			_, err := PlanPackageProjection(job, numbers, members, "export-dat-opt-images-v1", PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
			require.Error(t, err)
		})
	}
}

func TestPlanPackageProjectionRejectsUnrepresentablePageLabels(t *testing.T) {
	for _, sample := range []struct{ profile, label string }{
		{"export-dat-pdf-v1", "EX,0001"},
		{"export-dat-opt-images-v1", "EX®0001"},
		{"export-dat-lfp-images-v1", "EX;0001"},
		{"export-dat-lfp-images-v1", "EX@0001"},
	} {
		t.Run(sample.profile+"/"+sample.label, func(t *testing.T) {
			job, numbers, members := packageProjectionFixture(t)
			numbers.Numbers[0].Text = sample.label
			_, numbers.SHA256, _ = documentproduction.CanonicalNumberReservation(numbers)
			job.Receipt.NumberReservationSHA256 = numbers.SHA256
			resealPackageJob(t, &job)
			_, err := PlanPackageProjection(job, numbers, members, sample.profile,
				PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
			require.Error(t, err)
		})
	}
}

func TestPlanPackageProjectionRejectsNoncontiguousFamily(t *testing.T) {
	job, numbers, members := packageProjectionFixture(t)
	members[0].FamilyID = "family-one"
	members[1].FamilyID = "family-two"
	third := members[1]
	third.ID = "66666666-6666-4666-8666-666666666666"
	third.Ordinal = 3
	third.FamilyID = "family-one"
	members = append(members, third)
	numbers.Numbers = append(numbers.Numbers, documentproduction.AssignedNumber{
		MemberID: third.ID, MemberOrdinal: 3, Page: 1, Text: "ÉX-0004",
	})
	_, numbers.SHA256, _ = documentproduction.CanonicalNumberReservation(numbers)
	job.Receipt.NumberReservationSHA256 = numbers.SHA256
	for index, role := range []string{documentproduction.ArtifactRoleRedactedPDF,
		documentproduction.ArtifactRoleRedactedText, documentproduction.ArtifactRoleRedactedPage} {
		media := "application/pdf"
		switch index {
		case 1:
			media = "text/plain; charset=utf-8"
		case 2:
			media = "image/png"
		}
		page := 0
		if index == 2 {
			page = 1
		}
		job.Manifest.Artifacts = append(job.Manifest.Artifacts, documentproduction.Artifact{
			ID: packageArtifactID(8 + index), MemberID: third.ID, MemberOrdinal: 3,
			Page: page, Role: role, Path: fmt.Sprintf("private/third-%d", index),
			SHA256: testHash(role + third.ID), Size: 10, MediaType: media,
		})
	}
	resealPackageJob(t, &job)
	_, err := PlanPackageProjection(job, numbers, members, "export-dat-pdf-v1", PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
	require.Error(t, err)
}
