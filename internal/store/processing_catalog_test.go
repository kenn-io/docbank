package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	docsqlite "go.kenn.io/docbank/sqlite"
)

const (
	catalogSourceHash       = "23e1511d112436b653b117e5100acccd7ce990284141668cd99764e2f009f410"
	catalogEvidenceBlobHash = "48736e58b409ed0241c8d7bed9dc188a59e13002f48e7492850bec15b59147b6"
	catalogMarkdownBlobHash = "2afddc3dcd0a3b3c4e6d642f8b193018af46828a8f73265eb0330b0761e4354d"
	catalogBuildID          = "00000000000000000000000000000000000000000000000000000000000000b1"
	catalogBuildReplacement = "00000000000000000000000000000000000000000000000000000000000000b2"
	catalogAttachmentFirst  = "0000000000000000000000000000000000000000000000000000000000000051"
	catalogAttachmentSecond = "0000000000000000000000000000000000000000000000000000000000000052"
	catalogCapturedPolicy   = `{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"},{"max_count":1,"min_count":0,"role":"structured_evidence"}],"version":1}`
)

var catalogBlobContents = map[string][]byte{
	catalogSourceHash:       []byte("synthetic source pdf"),
	catalogEvidenceBlobHash: []byte("synthetic evidence"),
	catalogMarkdownBlobHash: []byte("synthetic markdown"),
}

func TestRenditionCatalogSharesOneBuildAcrossVersionProfilesWithinVault(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	baseProfile := catalogProcessingProfile(t, false)
	embeddingProfile := catalogProcessingProfile(t, true)
	build := catalogRenditionBuild(s, baseProfile)

	require.Equal(t, baseProfile.RenditionRequestFingerprint, embeddingProfile.RenditionRequestFingerprint)
	require.Equal(t, baseProfile.EvidenceLexicalFingerprint, embeddingProfile.EvidenceLexicalFingerprint)
	require.NotEqual(t, baseProfile.Fingerprint, embeddingProfile.Fingerprint,
		"an unrelated embedding binding must change the full profile only")
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))

	first := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: baseProfile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	second := RenditionAttachmentRecord{
		ID: catalogAttachmentSecond, VaultID: s.VaultID(),
		ContentVersionID: versions[1], BuildID: build.ID, Profile: embeddingProfile,
		AttachedAt: "2026-08-22T10:01:00.000000000Z",
	}
	require.NoError(t, publishRenditionForTest(
		t, s, first, "2026-08-22T10:02:00.000000000Z", fakeHash("c1"),
	))
	require.NoError(t, publishRenditionForTest(
		t, s, second, "2026-08-22T10:03:00.000000000Z", fakeHash("c1"),
	))

	firstView, err := s.ActiveRendition(t.Context(), versions[0], baseProfile.Fingerprint)
	require.NoError(t, err)
	secondView, err := s.ActiveRendition(t.Context(), versions[1], embeddingProfile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, build, firstView.Build)
	assert.Equal(t, build, secondView.Build)
	assert.Equal(t, first, firstView.Attachment)
	assert.Equal(t, second, secondView.Attachment)
	assert.Equal(t, catalogBuildID, firstView.Build.ID)
	assert.Equal(t, catalogBuildID, secondView.Build.ID)

	var buildCount, attachmentCount int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&buildCount))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_attachments`).Scan(&attachmentCount))
	assert.Equal(t, 1, buildCount)
	assert.Equal(t, 2, attachmentCount)
}

func TestRenditionCatalogRejectsCrossVaultAttachment(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	other := newTestStore(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))

	err := publishRenditionForTest(t, s, RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: other.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}, "2026-08-22T10:01:00.000000000Z", fakeHash("c2"))
	require.ErrorContains(t, err, "vault")

	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_attachments`).Scan(&count))
	assert.Zero(t, count)
}

func TestRenditionCatalogRejectsArtifactsForbiddenByAttachmentProfile(t *testing.T) {
	tests := []struct {
		name   string
		role   string
		policy jsontext.Value
		mutate func(*document.ProcessingProfileV1)
	}{
		{
			name: "sanitized Markdown retention disabled",
			mutate: func(profile *document.ProcessingProfileV1) {
				profile.RetentionDisclosure.RetainSanitizedMarkdown = false
			},
		},
		{
			name: "provider Markdown retention disabled",
			role: string(document.EvidenceArtifactMarkdown),
			policy: jsontext.Value(
				`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"provider_markdown"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`,
			),
			mutate: func(profile *document.ProcessingProfileV1) {
				profile.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{
					document.EvidenceArtifactMarkdown,
				}
			},
		},
		{
			name: "typed artifact retention disabled",
			role: string(document.EvidenceArtifactStructured),
			policy: jsontext.Value(
				`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"},{"max_count":1,"min_count":1,"role":"structured_evidence"}],"version":1}`,
			),
			mutate: func(profile *document.ProcessingProfileV1) {
				profile.RetentionDisclosure.RetainTypedArtifacts = false
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, versions := newRenditionCatalogFixture(t)
			profile := catalogProcessingProfileWith(t, false, tt.mutate)
			build := catalogRenditionBuild(s, profile)
			if tt.role != "" {
				build.CapturedArtifactPolicy = tt.policy
				build.CapturedArtifactPolicyFingerprint = testSHA256(tt.policy)
				build.Artifacts = append(build.Artifacts, RenditionArtifactRecord{
					ID: "artifact_" + fakeHash("2f"), Role: tt.role,
					BlobHash: catalogEvidenceBlobHash, Size: int64(len(catalogBlobContents[catalogEvidenceBlobHash])),
					Checksum: catalogEvidenceBlobHash, State: RenditionArtifactVerified,
				})
				build.DeclaredArtifactCount = len(build.Artifacts)
			}
			require.NoError(t, s.StageRenditionBuild(t.Context(), build))

			err := publishRenditionForTest(t, s, RenditionAttachmentRecord{
				ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
				BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-25T15:00:00.000000000Z",
			}, "2026-08-25T15:01:00.000000000Z", fakeHash("c3"))
			require.ErrorContains(t, err, "artifact role")

			var count int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_attachments`).Scan(&count))
			assert.Zero(t, count)
		})
	}
}

func TestProcessingMetadataRejectsRestoredAttachmentWithForbiddenArtifact(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfileWith(t, false, func(profile *document.ProcessingProfileV1) {
		profile.RetentionDisclosure.RetainTypedArtifacts = false
	})
	build := catalogRenditionBuild(s, profile)
	build.CapturedArtifactPolicy = jsontext.Value(
		`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"},{"max_count":1,"min_count":1,"role":"structured_evidence"}],"version":1}`,
	)
	build.CapturedArtifactPolicyFingerprint = testSHA256(build.CapturedArtifactPolicy)
	build.Artifacts = append(build.Artifacts, RenditionArtifactRecord{
		ID: "artifact_" + fakeHash("30"), Role: string(document.EvidenceArtifactStructured),
		BlobHash: catalogEvidenceBlobHash, Size: int64(len(catalogBlobContents[catalogEvidenceBlobHash])),
		Checksum: catalogEvidenceBlobHash, State: RenditionArtifactVerified,
	})
	build.DeclaredArtifactCount = len(build.Artifacts)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return ensureProcessingProfileTx(t.Context(), tx, profile)
	}))
	_, err := s.db.Exec(`INSERT INTO rendition_attachments(
		attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,
		retention_disclosure_fingerprint,attachment_policy_fingerprint,
		consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary,attached_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		catalogAttachmentFirst, s.VaultID(), versions[0], build.ID, profile.Fingerprint,
		profile.RetentionDisclosureFingerprint, profile.AttachmentPolicyFingerprint,
		profile.ConsentFingerprint, profile.RenditionDisclosureFingerprint, profile.TrustBoundary,
		"2026-08-25T15:10:00.000000000Z")
	require.NoError(t, err)

	require.ErrorContains(t, s.ValidateMetadata(t.Context()), "artifact role")
}

func TestRenditionCatalogRejectsIncompleteArtifactsWithoutPartialStage(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	build.DeclaredArtifactCount++

	err := s.StageRenditionBuild(t.Context(), build)
	require.ErrorContains(t, err, "artifact")

	var buildCount, artifactCount int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&buildCount))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_artifacts`).Scan(&artifactCount))
	assert.Zero(t, buildCount, "a rejected aggregate must not leave its parent row")
	assert.Zero(t, artifactCount, "a rejected aggregate must not leave child rows")

	err = publishRenditionForTest(t, s, RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}, "2026-08-22T10:01:00.000000000Z", fakeHash("c4"))
	require.Error(t, err)
	_, err = s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRenditionCatalogReservesLegacyProviderOperationForLegacyProfile(t *testing.T) {
	s, _ := newRenditionCatalogFixture(t)
	build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
	build.ProviderOperationID = legacyPlainTextProvider

	err := s.StageRenditionBuild(t.Context(), build)
	require.ErrorContains(t, err, "reserved for the legacy plain-text profile")
}

func TestRenditionCatalogValidatesEveryArtifactMembership(t *testing.T) {
	for name, mutate := range map[string]func(*RenditionArtifactRecord){
		"checksum disagreement": func(record *RenditionArtifactRecord) { record.Checksum = fakeHash("2f") },
		"size disagreement":     func(record *RenditionArtifactRecord) { record.Size++ },
		"unverified state":      func(record *RenditionArtifactRecord) { record.State = RenditionArtifactPending },
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newRenditionCatalogFixture(t)
			profile := catalogProcessingProfile(t, false)
			build := catalogRenditionBuild(s, profile)
			mutate(&build.Artifacts[0])

			err := s.StageRenditionBuild(t.Context(), build)
			require.Error(t, err)

			var count int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&count))
			assert.Zero(t, count)
		})
	}
}

func TestRenditionCatalogRequiresCapturedPolicyMembership(t *testing.T) {
	for name, mutate := range map[string]func(*RenditionBuildRecord){
		"missing required role": func(build *RenditionBuildRecord) {
			build.Artifacts = build.Artifacts[:1]
			build.DeclaredArtifactCount = 1
		},
		"extra role": func(build *RenditionBuildRecord) {
			build.Artifacts = append(build.Artifacts, RenditionArtifactRecord{
				ID: "artifact_" + fakeHash("23"), Role: "provider_image",
				BlobHash: catalogEvidenceBlobHash, Size: int64(len(catalogBlobContents[catalogEvidenceBlobHash])),
				Checksum: catalogEvidenceBlobHash, State: RenditionArtifactVerified,
			})
			build.DeclaredArtifactCount = len(build.Artifacts)
		},
		"per-role overflow": func(build *RenditionBuildRecord) {
			build.CapturedArtifactPolicy = jsontext.Value(
				`{"roles":[{"max_count":1,"min_count":0,"role":"provider_image"}],"version":1}`,
			)
			build.CapturedArtifactPolicyFingerprint = testSHA256(build.CapturedArtifactPolicy)
			build.Artifacts = []RenditionArtifactRecord{
				{ID: "artifact_" + fakeHash("23"), Role: "provider_image", BlobHash: catalogEvidenceBlobHash,
					Size:     int64(len(catalogBlobContents[catalogEvidenceBlobHash])),
					Checksum: catalogEvidenceBlobHash, State: RenditionArtifactVerified},
				{ID: "artifact_" + fakeHash("24"), Role: "provider_image", BlobHash: catalogEvidenceBlobHash,
					Size:     int64(len(catalogBlobContents[catalogEvidenceBlobHash])),
					Checksum: catalogEvidenceBlobHash, State: RenditionArtifactVerified},
			}
			build.DeclaredArtifactCount = len(build.Artifacts)
		},
		"unknown policy role": func(build *RenditionBuildRecord) {
			build.CapturedArtifactPolicy = jsontext.Value(
				`{"roles":[{"max_count":1,"min_count":1,"role":"unknown"}],"version":1}`,
			)
			build.CapturedArtifactPolicyFingerprint = testSHA256(build.CapturedArtifactPolicy)
			build.Artifacts = build.Artifacts[:1]
			build.Artifacts[0].Role = "unknown"
			build.DeclaredArtifactCount = 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newRenditionCatalogFixture(t)
			build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
			mutate(&build)

			err := s.StageRenditionBuild(t.Context(), build)
			require.Error(t, err)

			var count int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&count))
			assert.Zero(t, count)
		})
	}
}

func TestRenditionCatalogSupportsCapturedArtifactCardinality(t *testing.T) {
	for name, arrange := range map[string]func(*RenditionBuildRecord){
		"zero retention": func(build *RenditionBuildRecord) {
			build.CapturedArtifactPolicy = jsontext.Value(`{"roles":[],"version":1}`)
			build.Artifacts = nil
			build.DeclaredArtifactCount = 0
		},
		"optional absent": func(build *RenditionBuildRecord) {
			build.CapturedArtifactPolicy = jsontext.Value(
				`{"roles":[{"max_count":1,"min_count":0,"role":"normalized_evidence"}],"version":1}`,
			)
			build.Artifacts = nil
			build.DeclaredArtifactCount = 0
		},
		"portable maximum absent": func(build *RenditionBuildRecord) {
			build.CapturedArtifactPolicy = jsontext.Value(
				`{"roles":[{"max_count":1024,"min_count":0,"role":"provider_image"}],"version":1}`,
			)
			build.Artifacts = nil
			build.DeclaredArtifactCount = 0
		},
		"optional present": func(build *RenditionBuildRecord) {
			build.CapturedArtifactPolicy = jsontext.Value(
				`{"roles":[{"max_count":1,"min_count":0,"role":"normalized_evidence"}],"version":1}`,
			)
			build.Artifacts = build.Artifacts[:1]
			build.DeclaredArtifactCount = 1
		},
		"multiple provider images": func(build *RenditionBuildRecord) {
			build.CapturedArtifactPolicy = jsontext.Value(
				`{"roles":[{"max_count":2,"min_count":0,"role":"provider_image"}],"version":1}`,
			)
			build.Artifacts = []RenditionArtifactRecord{
				{ID: "artifact_" + fakeHash("23"), Role: "provider_image", BlobHash: catalogEvidenceBlobHash,
					Size:     int64(len(catalogBlobContents[catalogEvidenceBlobHash])),
					Checksum: catalogEvidenceBlobHash, State: RenditionArtifactVerified},
				{ID: "artifact_" + fakeHash("24"), Role: "provider_image", BlobHash: catalogEvidenceBlobHash,
					Size:     int64(len(catalogBlobContents[catalogEvidenceBlobHash])),
					Checksum: catalogEvidenceBlobHash, State: RenditionArtifactVerified},
			}
			build.DeclaredArtifactCount = 2
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newRenditionCatalogFixture(t)
			build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
			arrange(&build)
			build.CapturedArtifactPolicyFingerprint = testSHA256(build.CapturedArtifactPolicy)

			require.NoError(t, s.StageRenditionBuild(t.Context(), build))

			var artifacts int
			require.NoError(t, s.db.QueryRow(
				`SELECT COUNT(*) FROM rendition_artifacts WHERE build_id=?`, build.ID,
			).Scan(&artifacts))
			assert.Equal(t, build.DeclaredArtifactCount, artifacts)
		})
	}
}

func TestRenditionCatalogRejectsInvalidCapturedArtifactCardinality(t *testing.T) {
	for name, policy := range map[string]jsontext.Value{
		"missing roles field":   jsontext.Value(`{"version":1}`),
		"null roles field":      jsontext.Value(`{"roles":null,"version":1}`),
		"missing minimum field": jsontext.Value(`{"roles":[{"max_count":1,"role":"provider_image"}],"version":1}`),
		"null minimum field":    jsontext.Value(`{"roles":[{"max_count":1,"min_count":null,"role":"provider_image"}],"version":1}`),
		"duplicate policy rule": jsontext.Value(
			`{"roles":[{"max_count":1,"min_count":0,"role":"provider_image"},{"max_count":2,"min_count":0,"role":"provider_image"}],"version":1}`,
		),
		"negative minimum": jsontext.Value(
			`{"roles":[{"max_count":1,"min_count":-1,"role":"provider_image"}],"version":1}`,
		),
		"minimum above maximum": jsontext.Value(
			`{"roles":[{"max_count":1,"min_count":2,"role":"provider_image"}],"version":1}`,
		),
		"role maximum above catalog maximum": jsontext.Value(
			`{"roles":[{"max_count":1025,"min_count":0,"role":"provider_image"}],"version":1}`,
		),
		"summed maximum above catalog maximum": jsontext.Value(
			`{"roles":[{"max_count":512,"min_count":0,"role":"provider_image"},{"max_count":513,"min_count":0,"role":"provider_markdown"}],"version":1}`,
		),
		"unknown role": jsontext.Value(
			`{"roles":[{"max_count":1,"min_count":0,"role":"unknown"}],"version":1}`,
		),
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newRenditionCatalogFixture(t)
			build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
			build.CapturedArtifactPolicy = policy
			fingerprintPolicy := policy
			switch name {
			case "missing roles field", "null roles field":
				fingerprintPolicy = jsontext.Value(`{"roles":[],"version":1}`)
			case "missing minimum field", "null minimum field":
				fingerprintPolicy = jsontext.Value(
					`{"roles":[{"max_count":1,"min_count":0,"role":"provider_image"}],"version":1}`,
				)
			}
			build.CapturedArtifactPolicyFingerprint = testSHA256(fingerprintPolicy)
			build.Artifacts = nil
			build.DeclaredArtifactCount = 0

			require.Error(t, s.StageRenditionBuild(t.Context(), build))

			var builds int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_builds`).Scan(&builds))
			assert.Zero(t, builds)
		})
	}
}

func TestRenditionCatalogCanonicalizesCapturedArtifactRuleOrder(t *testing.T) {
	s, _ := newRenditionCatalogFixture(t)
	build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
	build.CapturedArtifactPolicy = jsontext.Value(
		`{"roles":[{"max_count":1,"min_count":0,"role":"structured_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"},{"max_count":1,"min_count":1,"role":"normalized_evidence"}],"version":1}`,
	)
	build.CapturedArtifactPolicyFingerprint = testSHA256([]byte(catalogCapturedPolicy))

	require.NoError(t, s.StageRenditionBuild(t.Context(), build))

	var stored string
	require.NoError(t, s.db.QueryRow(
		`SELECT captured_artifact_policy_json FROM rendition_builds WHERE build_id=?`, build.ID,
	).Scan(&stored))
	assert.JSONEq(t, catalogCapturedPolicy, stored)

	build.CapturedArtifactPolicy = jsontext.Value(catalogCapturedPolicy)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build),
		"equivalent policy order must exactly reuse the immutable build")
}

func TestRenditionCatalogRejectsInvalidUnitLocators(t *testing.T) {
	for name, locator := range map[string]document.EvidenceLocatorV1{
		"unknown kind": {
			Kind: "unknown", IndexOrigin: document.EvidenceIndexOriginNone,
		},
		"indexed kind without origin": {
			Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginNone,
			Start: 1, End: 1,
		},
		"single unit kind with range": {
			Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
			Start: 1, End: 2,
		},
		"sheet without name": {
			Kind: document.EvidenceLocatorSheet, IndexOrigin: document.EvidenceIndexOriginOne,
			Start: 1, End: 1,
		},
		"oversized name": {
			Kind: document.EvidenceLocatorSection, IndexOrigin: document.EvidenceIndexOriginNone,
			Name: strings.Repeat("n", 1025),
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newRenditionCatalogFixture(t)
			build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
			build.Units[0].Locator = locator

			err := s.StageRenditionBuild(t.Context(), build)
			require.Error(t, err)
		})
	}
}

func TestRenditionCatalogPortableLimitsRejectMaxPlusOne(t *testing.T) {
	s, _ := newRenditionCatalogFixture(t)
	base := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
	value := metadataRenditionBuild{
		Type: metadataRenditionBuildType, ID: base.ID, VaultID: base.VaultID,
		SourceSHA256:                      base.SourceSHA256,
		RenditionRequestFingerprint:       base.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        base.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: base.CapturedArtifactPolicyFingerprint,
		CapturedArtifactPolicy:            base.CapturedArtifactPolicy,
		AuthorizationChecksum:             base.AuthorizationChecksum, ProviderOperationID: base.ProviderOperationID,
		ProviderReceipt: base.ProviderReceipt, EvidenceChecksum: base.EvidenceChecksum,
		RenditionChecksum: base.RenditionChecksum, MarkdownChecksum: base.MarkdownChecksum,
		Completeness: base.Completeness, Warnings: base.Warnings, CompletedAt: base.CompletedAt,
	}

	for name, counts := range map[string][3]int{
		"artifact count": {1024, 0, 0},
		"unit count":     {0, 100_000, 0},
		"segment count":  {0, 0, 1_000_000},
	} {
		t.Run(name, func(t *testing.T) {
			atMax := value
			atMax.DeclaredArtifactCount, atMax.UnitCount, atMax.LexicalSegmentCount = counts[0], counts[1], counts[2]
			require.NoError(t, validateMetadataRenditionBuild(atMax))

			over := atMax
			switch name {
			case "artifact count":
				over.DeclaredArtifactCount++
			case "unit count":
				over.UnitCount++
			case "segment count":
				over.LexicalSegmentCount++
			}
			require.Error(t, validateMetadataRenditionBuild(over))
		})
	}

	atStringMax := value
	atStringMax.ProviderOperationID = strings.Repeat("o", 4096)
	require.NoError(t, validateMetadataRenditionBuild(atStringMax))
	overStringMax := atStringMax
	overStringMax.ProviderOperationID += "o"
	require.Error(t, validateMetadataRenditionBuild(overStringMax))
	reservedOperation := value
	reservedOperation.ProviderOperationID = legacyPlainTextProvider
	require.ErrorContains(t, validateMetadataRenditionBuild(reservedOperation),
		"reserved for the legacy plain-text profile")

	jsonPrefix, jsonSuffix := `{"receipt":"`, `"}`
	atJSONMax := value
	atJSONMax.ProviderReceipt = jsontext.Value(
		jsonPrefix + strings.Repeat("r", (1<<20)-len(jsonPrefix)-len(jsonSuffix)) + jsonSuffix,
	)
	require.Len(t, atJSONMax.ProviderReceipt, 1<<20)
	require.NoError(t, validateMetadataRenditionBuild(atJSONMax))
	overJSONMax := atJSONMax
	overJSONMax.ProviderReceipt = jsontext.Value(
		jsonPrefix + strings.Repeat("r", (1<<20)-len(jsonPrefix)-len(jsonSuffix)+1) + jsonSuffix,
	)
	require.Error(t, validateMetadataRenditionBuild(overJSONMax))

	atCollectionMax := value
	atCollectionMax.Warnings = make([]string, 256)
	require.NoError(t, validateMetadataRenditionBuild(atCollectionMax))
	overCollectionMax := atCollectionMax
	overCollectionMax.Warnings = append(overCollectionMax.Warnings, "")
	require.Error(t, validateMetadataRenditionBuild(overCollectionMax))

	atTextMax := metadataRenditionSegment{
		Type: metadataRenditionSegmentType, BuildID: catalogBuildID,
		SegmentID: "segment_" + fakeHash("33"), UnitID: "rendition_unit_" + fakeHash("31"),
		CharEnd: maxLexicalSegmentRunes, Checksum: fakeHash("33"),
		Text: strings.Repeat("s", maxLexicalSegmentRunes),
	}
	require.NoError(t, validateMetadataRenditionSegment(atTextMax))
	overTextMax := atTextMax
	overTextMax.Text += "s"
	overTextMax.CharEnd++
	require.Error(t, validateMetadataRenditionSegment(overTextMax))
}

func TestRenditionCatalogAPIRejectsPortableLimitOverflows(t *testing.T) {
	for name, mutate := range map[string]func(*RenditionBuildRecord){
		"provider operation ID": func(build *RenditionBuildRecord) {
			build.ProviderOperationID = strings.Repeat("o", 4097)
		},
		"provider receipt JSON": func(build *RenditionBuildRecord) {
			build.ProviderReceipt = jsontext.Value(`{"receipt":"` + strings.Repeat("r", 1<<20) + `"}`)
		},
		"warning collection": func(build *RenditionBuildRecord) {
			build.Warnings = make([]string, 257)
		},
		"warning text": func(build *RenditionBuildRecord) {
			build.Warnings = []string{strings.Repeat("w", 4097)}
		},
		"artifact ID": func(build *RenditionBuildRecord) {
			build.Artifacts[0].ID = strings.Repeat("a", 1025)
		},
		"heading collection": func(build *RenditionBuildRecord) {
			build.Units[0].HeadingPath = make([]string, 65)
			for index := range build.Units[0].HeadingPath {
				build.Units[0].HeadingPath[index] = "heading"
			}
		},
		"heading text": func(build *RenditionBuildRecord) {
			build.Units[0].HeadingPath = []string{strings.Repeat("h", 4097)}
		},
		"lexical text": func(build *RenditionBuildRecord) {
			build.LexicalSegments[0].Text = strings.Repeat("s", (1<<20)+1)
			build.LexicalSegments[0].CharEnd = len(build.LexicalSegments[0].Text)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newRenditionCatalogFixture(t)
			build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
			mutate(&build)

			require.Error(t, s.StageRenditionBuild(t.Context(), build))
		})
	}
}

func TestProcessingMetadataRejectsInvalidLocatorBeforeInsert(t *testing.T) {
	value := metadataRenditionUnit{
		Type: metadataRenditionUnitType, BuildID: catalogBuildID,
		UnitID: "rendition_unit_" + fakeHash("31"), EvidenceUnitID: "unit_" + fakeHash("32"),
		Checksum: fakeHash("31"), HeadingPath: []string{},
		Locator: document.EvidenceLocatorV1{
			Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginNone,
			Start: 1, End: 1,
		},
	}
	require.Error(t, validateMetadataRenditionUnit(value))
}

func TestProcessingMetadataRejectsUnknownCapturedPolicyBeforeInsert(t *testing.T) {
	s, _ := newRenditionCatalogFixture(t)
	build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
	policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":0,"role":"unknown"}],"version":1}`)
	value := metadataRenditionBuild{
		Type: metadataRenditionBuildType, ID: build.ID, VaultID: build.VaultID,
		SourceSHA256:                      build.SourceSHA256,
		RenditionRequestFingerprint:       build.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        build.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: testSHA256(policy), CapturedArtifactPolicy: policy,
		AuthorizationChecksum: build.AuthorizationChecksum, ProviderOperationID: build.ProviderOperationID,
		ProviderReceipt: build.ProviderReceipt, EvidenceChecksum: build.EvidenceChecksum,
		RenditionChecksum: build.RenditionChecksum, MarkdownChecksum: build.MarkdownChecksum,
		Completeness: build.Completeness, Warnings: build.Warnings, CompletedAt: build.CompletedAt,
	}
	require.Error(t, validateMetadataRenditionBuild(value))
}

func TestProcessingMetadataOpenRejectsPartialSchemas(t *testing.T) {
	for name, tablesToDrop := range map[string][]string{
		"one of seven tables": {
			"rendition_heads", "rendition_attachments", "rendition_lexical_segments",
			"rendition_units", "rendition_artifacts", "rendition_builds",
		},
		"six of seven tables":                {"rendition_heads"},
		"missing current roots":              {"current_rendition_roots"},
		"missing purge suppressions":         {"derivative_purge_suppressions"},
		"missing provider staging":           {"rendition_blob_staging"},
		"missing pending derivative erasure": {"derivative_blob_purge_pending"},
		"missing pending pack erasure":       {"derivative_pack_purge_pending"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "partial.db")
			driver := DefaultSQLiteDriver()
			current, err := openCurrentStore(path, driver)
			require.NoError(t, err)
			require.NoError(t, current.Close())
			db, err := driver.Open(path, docsqlite.OpenOptions{
				Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
			})
			require.NoError(t, err)
			for _, table := range tablesToDrop {
				_, err = db.Exec(`DROP TABLE ` + table)
				require.NoError(t, err)
			}
			require.NoError(t, db.Close())

			opened, err := openCurrentStore(path, driver)
			if opened != nil {
				require.NoError(t, opened.Close())
			}
			require.ErrorContains(t, err, "processing")
		})
	}
}

func TestProcessingMetadataOpenRejectsPartialLexicalSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial-lexical.db")
	driver := DefaultSQLiteDriver()
	current, err := openCurrentStore(path, driver)
	require.NoError(t, err)
	_, err = current.db.Exec(lexicalProjectionSchema)
	require.NoError(t, err)
	require.NoError(t, current.Close())

	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.Exec(`DROP TABLE rendition_lexical_generation_builds`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	opened, err := openCurrentStore(path, driver)
	if opened != nil {
		require.NoError(t, opened.Close())
	}
	require.ErrorContains(t, err, "lexical")
}

func TestProcessingMetadataOpenAcceptsCRLFEmbeddedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crlf-schema.db")
	driver := DefaultSQLiteDriver()
	originalSchema := schemaSQL
	schemaSQL = strings.ReplaceAll(schemaSQL, "\r\n", "\n")
	schemaSQL = strings.ReplaceAll(schemaSQL, "\n", "\r\n")
	t.Cleanup(func() { schemaSQL = originalSchema })
	current, err := openCurrentStore(path, driver)
	require.NoError(t, err)
	_, err = current.db.Exec(lexicalProjectionSchema)
	require.NoError(t, err)
	require.NoError(t, current.Close())

	reopened, err := openCurrentStore(path, driver)
	if reopened != nil {
		require.NoError(t, reopened.Close())
	}
	require.NoError(t, err,
		"checkout line endings must not change exact lexical schema identity")
}

func TestProcessingMetadataOpenRejectsMismatchedCompleteSchemas(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, string, docsqlite.Driver){
		"current layout with missing bound": func(t *testing.T, path string, driver docsqlite.Driver) {
			t.Helper()
			corruptProcessingSchemaCheckForTest(
				t, path, driver, "rendition_builds", "CHECK (declared_artifact_count >= 0)")
		},
		"current layout with weakened consent bound": func(t *testing.T, path string, driver docsqlite.Driver) {
			t.Helper()
			corruptProcessingSchemaCheckForTest(
				t, path, driver, "processing_consent_grants", "CHECK (revocation_fence >= 0)")
		},
		"current layout with weakened job bound": func(t *testing.T, path string, driver docsqlite.Driver) {
			t.Helper()
			corruptProcessingSchemaCheckForTest(
				t, path, driver, "rendition_jobs", "CHECK (provider_attempts >= 0)")
		},
		"current layout with extra trigger": func(t *testing.T, path string, driver docsqlite.Driver) {
			t.Helper()
			addProcessingSchemaTriggerForTest(t, path, driver)
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mismatched.db")
			driver := DefaultSQLiteDriver()
			current, err := openCurrentStore(path, driver)
			require.NoError(t, err)
			require.NoError(t, current.Close())
			mutate(t, path, driver)

			opened, err := openCurrentStore(path, driver)
			if opened != nil {
				require.NoError(t, opened.Close())
			}
			require.ErrorContains(t, err, "processing")
		})
	}
}

func TestProcessingMetadataOpenAcceptsExactCurrentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	driver := DefaultSQLiteDriver()
	created, err := openCurrentStore(path, driver)
	require.NoError(t, err)
	require.NoError(t, created.Close())

	reopened, err := openCurrentStore(path, driver)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

func TestRenditionCatalogInsertOrReuseRejectsImmutableConflicts(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	build := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	require.NoError(t, s.StageRenditionBuild(t.Context(), build), "an exact retry must reuse the immutable aggregate")
	reorderedBuild := cloneCatalogBuild(build)
	reorderedBuild.Artifacts[0], reorderedBuild.Artifacts[1] =
		reorderedBuild.Artifacts[1], reorderedBuild.Artifacts[0]
	require.NoError(t, s.StageRenditionBuild(t.Context(), reorderedBuild),
		"artifact membership order must not change exact build content")

	conflictingBuild := cloneCatalogBuild(build)
	conflictingBuild.ProviderOperationID = "synthetic-operation-conflict"
	err := s.StageRenditionBuild(t.Context(), conflictingBuild)
	require.ErrorContains(t, err, "different immutable")

	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	require.NoError(t, publishRenditionForTest(
		t, s, attachment, "2026-08-22T10:01:00.000000000Z", fakeHash("c5"),
	))
	require.NoError(t, publishRenditionForTest(
		t, s, attachment, "2026-08-22T10:01:00.000000000Z", fakeHash("c5"),
	), "an exact retry must reuse the immutable attachment")

	conflictingAttachment := attachment
	conflictingAttachment.AttachedAt = "2026-08-22T10:00:01.000000000Z"
	err = publishRenditionForTest(
		t, s, conflictingAttachment, "2026-08-22T10:01:00.000000000Z", fakeHash("c5"),
	)
	require.ErrorContains(t, err, "different immutable")

	var providerOperationID string
	require.NoError(t, s.db.QueryRow(
		`SELECT provider_operation_id FROM rendition_builds WHERE build_id = ?`, build.ID,
	).Scan(&providerOperationID))
	assert.Equal(t, build.ProviderOperationID, providerOperationID)
}

func TestRenditionCatalogExactRetryCanonicalizesEmptyBuildCollections(t *testing.T) {
	s, _ := newRenditionCatalogFixture(t)
	build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
	build.CapturedArtifactPolicy = jsontext.Value(`{"roles":[],"version":1}`)
	build.CapturedArtifactPolicyFingerprint = testSHA256(build.CapturedArtifactPolicy)
	build.DeclaredArtifactCount = 0
	build.Artifacts = []RenditionArtifactRecord{}
	build.Units = []RenditionUnitRecord{}
	build.LexicalSegments = []RenditionLexicalSegmentRecord{}

	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	require.NoError(t, s.StageRenditionBuild(t.Context(), build),
		"an exact retry must reuse an aggregate with empty child collections")
}

func TestRenditionCatalogFailedReplacementKeepsOldHead(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	oldBuild := catalogRenditionBuild(s, profile)
	require.NoError(t, s.StageRenditionBuild(t.Context(), oldBuild))
	oldAttachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: oldBuild.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	require.NoError(t, publishRenditionForTest(
		t, s, oldAttachment, "2026-08-22T10:01:00.000000000Z", fakeHash("c6"),
	))

	replacement := cloneCatalogBuild(oldBuild)
	replacement.ID = catalogBuildReplacement
	replacement.CapturedArtifactPolicy = jsontext.Value(
		`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":0,"role":"sanitized_markdown"}],"version":1}`,
	)
	replacement.CapturedArtifactPolicyFingerprint = testSHA256(replacement.CapturedArtifactPolicy)
	replacement.ProviderOperationID = "synthetic-operation-replacement"
	replacement.CompletedAt = "2026-08-22T11:00:00.000000000Z"
	require.NoError(t, s.StageRenditionBuild(t.Context(), replacement))
	newAttachment := RenditionAttachmentRecord{
		ID: catalogAttachmentSecond, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: replacement.ID, Profile: profile, AttachedAt: "2026-08-22T11:01:00.000000000Z",
	}
	_, err := s.db.Exec(`CREATE TRIGGER synthetic_reject_rendition_head_update
		BEFORE UPDATE ON rendition_heads BEGIN
			SELECT RAISE(ABORT, 'synthetic head publication failure');
		END`)
	require.NoError(t, err)
	err = publishRenditionForTest(
		t, s, newAttachment, "2026-08-22T11:02:00.000000000Z", fakeHash("c7"),
	)
	require.ErrorContains(t, err, "synthetic head publication failure")

	active, err := s.ActiveRendition(t.Context(), versions[0], profile.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, oldBuild.ID, active.Build.ID)
	assert.Equal(t, oldAttachment.ID, active.Attachment.ID)
	assert.Equal(t, oldAttachment.ID, active.Head.AttachmentID)
}

func newRenditionCatalogFixture(t *testing.T) (*Store, []string) {
	t.Helper()
	s := newTestStore(t)
	return s, seedRenditionCatalogVersions(t, s)
}

func publishRenditionForTest(
	t *testing.T, s *Store, attachment RenditionAttachmentRecord, publishedAt, generationID string,
) error {
	t.Helper()
	generation, err := s.StageLexicalGeneration(t.Context(), generationID)
	require.NoError(t, err)
	return s.PublishRenditionAndLexicalHeads(t.Context(), attachment, RenditionHeadRecord{
		ContentVersionID:             attachment.ContentVersionID,
		ProcessingProfileFingerprint: attachment.Profile.Fingerprint,
		AttachmentID:                 attachment.ID,
		PublishedAt:                  publishedAt,
	}, generation.ID)
}

func publishAttachmentForTest(
	t *testing.T, s *Store, attachment RenditionAttachmentRecord,
) error {
	t.Helper()
	var generationID string
	err := s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		rows, err := readCatalogLexicalManifestRowsTx(t.Context(), tx, "")
		if err != nil {
			return err
		}
		buildIDs, err := lexicalCatalogBuildIDsTx(t.Context(), tx)
		if err != nil {
			return err
		}
		generationID = lexicalReplacementGenerationID(rows, buildIDs)
		return nil
	})
	if err != nil {
		return err
	}
	return publishRenditionForTest(
		t, s, attachment, nowRFC3339(), generationID,
	)
}

func seedRenditionCatalogVersions(t *testing.T, s *Store) []string {
	t.Helper()
	first, err := s.CreateFile(t.Context(), s.RootID(), "synthetic-source-a.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	second, err := s.CreateFile(t.Context(), s.RootID(), "synthetic-source-b.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		if err := s.EnsureBlobTx(tx, catalogEvidenceBlobHash, int64(len(catalogBlobContents[catalogEvidenceBlobHash]))); err != nil {
			return err
		}
		return s.EnsureBlobTx(tx, catalogMarkdownBlobHash, int64(len(catalogBlobContents[catalogMarkdownBlobHash])))
	}))
	return []string{first.CurrentVersionID, second.CurrentVersionID}
}

func catalogProcessingProfile(t *testing.T, withEmbedding bool) ProcessingProfileRecord {
	t.Helper()
	return catalogProcessingProfileWith(t, withEmbedding, nil)
}

func catalogProcessingProfileWith(
	t *testing.T, withEmbedding bool, mutate func(*document.ProcessingProfileV1),
) ProcessingProfileRecord {
	t.Helper()
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "rendition-adapter/v1", AuthorizationFingerprint: fakeHash("a1"),
			CredentialBinding: "credential:catalog", DeploymentFingerprint: fakeHash("a2"),
			Descriptor:            document.ProviderDescriptorV1{ID: "synthetic-rendition", Fingerprint: fakeHash("a3")},
			DisclosureFingerprint: fakeHash("a4"), MaxDocumentBytes: 1 << 20,
			MaxResponseBytes: 1 << 20, MaxUnits: 100, Name: "primary",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      "synthetic-vault", UploadOptionsFingerprint: fakeHash("a5"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: fakeHash("b1"), LexicalSegmenterFingerprint: fakeHash("b2"),
			MaxDocumentChars: 100_000, MaxSegmentRunes: 100, MaxUnitRunes: 1000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      fakeHash("b3"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: fakeHash("b4"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: fakeHash("c1"), ConsentFingerprint: fakeHash("c2"),
			RetainSanitizedMarkdown: true, RetainTypedArtifacts: true, TrustBoundary: "synthetic-vault",
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
	}
	if withEmbedding {
		profile.Embeddings = []document.EmbeddingBindingV1{{
			Activation: document.EmbeddingOptional, AuthorizationFingerprint: fakeHash("d1"),
			CompatibilityID: "synthetic-space", CredentialBinding: "credential:catalog-embedding",
			Descriptor: document.ProviderDescriptorV1{ID: "synthetic-embedding", Fingerprint: fakeHash("d2")},
			Dimensions: 8, DisclosureFingerprint: fakeHash("d3"), DocumentFormatter: "document/v1",
			InputKind: document.EmbeddingInputOriginalFile, MaxBatchItems: 8, MaxInputBytes: 1 << 20,
			MaxResponseBytes: 1 << 20, Metric: "cosine", Model: "synthetic-v1", Name: "optional",
			Normalization: "none", QueryFormatter: "query/v1", ScalarEncoding: "float32_le",
			TrustBoundary: "synthetic-vault",
		}}
	}
	if mutate != nil {
		mutate(&profile)
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
}

func catalogRenditionBuild(s *Store, profile ProcessingProfileRecord) RenditionBuildRecord {
	policy := jsontext.Value(catalogCapturedPolicy)
	return RenditionBuildRecord{
		ID: catalogBuildID, VaultID: s.VaultID(), SourceSHA256: catalogSourceHash,
		RenditionRequestFingerprint:       profile.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        profile.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: testSHA256(policy), CapturedArtifactPolicy: policy,
		AuthorizationChecksum: fakeHash("a6"), ProviderOperationID: "synthetic-operation",
		ProviderReceipt:  jsontext.Value(`{"provider":"synthetic","request_id":"request-1"}`),
		EvidenceChecksum: fakeHash("e1"), RenditionChecksum: fakeHash("e2"),
		MarkdownChecksum: catalogMarkdownBlobHash, Completeness: document.EvidenceComplete,
		Warnings: []string{"synthetic warning"}, CompletedAt: "2026-08-22T09:00:00.000000000Z",
		DeclaredArtifactCount: 2,
		Artifacts: []RenditionArtifactRecord{
			{ID: "artifact_" + fakeHash("21"), Role: "normalized_evidence", BlobHash: catalogEvidenceBlobHash,
				Size: int64(len(catalogBlobContents[catalogEvidenceBlobHash])), Checksum: catalogEvidenceBlobHash, State: RenditionArtifactVerified},
			{ID: "artifact_" + fakeHash("22"), Role: "sanitized_markdown", BlobHash: catalogMarkdownBlobHash,
				Size: int64(len(catalogBlobContents[catalogMarkdownBlobHash])), Checksum: catalogMarkdownBlobHash, State: RenditionArtifactVerified},
		},
		Units: []RenditionUnitRecord{{
			ID: "rendition_unit_" + fakeHash("31"), EvidenceUnitID: "unit_" + fakeHash("32"),
			Order: 0, Checksum: fakeHash("31"), HeadingPath: []string{"Synthetic report"},
			Locator: document.EvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1},
		}},
		LexicalSegments: []RenditionLexicalSegmentRecord{{
			ID: "lexical_segment_" + fakeHash("41"), UnitID: "rendition_unit_" + fakeHash("31"),
			Order: 0, CharStart: 0, CharEnd: 16, Checksum: fakeHash("41"), Text: "Synthetic report",
		}},
	}
}

func cloneCatalogBuild(build RenditionBuildRecord) RenditionBuildRecord {
	clone := build
	clone.CapturedArtifactPolicy = append(jsontext.Value(nil), build.CapturedArtifactPolicy...)
	clone.ProviderReceipt = append(jsontext.Value(nil), build.ProviderReceipt...)
	clone.Warnings = append([]string(nil), build.Warnings...)
	clone.Artifacts = append([]RenditionArtifactRecord(nil), build.Artifacts...)
	clone.Units = append([]RenditionUnitRecord(nil), build.Units...)
	clone.LexicalSegments = append([]RenditionLexicalSegmentRecord(nil), build.LexicalSegments...)
	for index := range clone.Units {
		clone.Units[index].HeadingPath = append([]string(nil), build.Units[index].HeadingPath...)
	}
	return clone
}

func testSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func corruptProcessingSchemaCheckForTest(
	t *testing.T, path string, driver docsqlite.Driver, table, check string,
) {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	require.NoError(t, db.Ping())
	_, err = db.Exec(`PRAGMA writable_schema = ON`)
	require.NoError(t, err)
	result, err := db.Exec(`
		UPDATE sqlite_schema
		SET sql=replace(sql, ?, 'CHECK (1)')
		WHERE type='table' AND name=? AND instr(sql, ?) > 0`, check, table, check)
	require.NoError(t, err)
	changed, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	_, err = db.Exec(`PRAGMA writable_schema = OFF`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func addProcessingSchemaTriggerForTest(t *testing.T, path string, driver docsqlite.Driver) {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TRIGGER processing_schema_extra_trigger
		BEFORE DELETE ON rendition_heads BEGIN
			SELECT RAISE(ABORT, 'unexpected processing trigger');
		END`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}
