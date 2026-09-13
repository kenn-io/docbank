package document

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
)

func testEmailRecipe() EmailRecipeV1 {
	return EmailRecipeV1{ContractVersion: EmailRecipeContractV1, ImplementationRevision: 1, GoVersion: "go1.27.0", CharsetProfile: "docbank-email-charset/v1", HeaderProfile: "docbank-email-header/v1", FilenameProfile: "docbank-email-filename/v1", BodyProfile: "docbank-email-body-selection/v1", Limits: canonicalEmailLimits()}
}

func unavailableEmail() EmailV1 {
	limit := int64(128 << 20)
	observed := limit + 1
	return EmailV1{ContractVersion: EmailContractV1, Source: EmailSourceV1{SHA256: strings.Repeat("a", 64), Size: observed, Verification: EmailVerificationCatalogOnly}, Recipe: testEmailRecipe(), Outcome: EmailOutcomeUnavailable, Failure: &EmailFailureV1{Code: EmailDiagnosticSourceSizeLimit, Operation: EmailOperationSource, Limit: &limit, Observed: &observed, Detail: "source too large"}}
}

func TestMarshalEmailV1HasIndependentCanonicalBytes(t *testing.T) {
	value := unavailableEmail()
	encoded, checksum, err := MarshalEmailV1(value)
	require.NoError(t, err)
	expected := `{"contract_version":"docbank-email/v1","failure":{"code":"source_size_limit","detail":"source too large","limit":134217728,"observed":134217729,"operation":"source","path":null},"inventory":null,"outcome":"unavailable","recipe":{"body_profile":"docbank-email-body-selection/v1","charset_profile":"docbank-email-charset/v1","contract_version":"docbank-email-decoder/v1","filename_profile":"docbank-email-filename/v1","go_version":"go1.27.0","header_profile":"docbank-email-header/v1","implementation_revision":1,"limits":{"aggregate_body_utf8_bytes":268435456,"aggregate_header_bytes":8388608,"aggregate_header_fields":16384,"body_utf8_bytes":16777216,"decoded_bytes":268435456,"depth":16,"diagnostic_detail_bytes":1024,"diagnostics":4096,"header_bytes":1048576,"header_display_bytes":1048576,"header_fields":4096,"html_display_bytes":16777216,"inventory_bytes":8388608,"part_bytes":134217728,"parts":1000,"source_bytes":134217728}},"source":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":134217729,"verification":"catalog_only"}}`
	assert.Equal(t, expected, string(encoded))
	sum := sha256.Sum256([]byte(expected))
	assert.Equal(t, hex.EncodeToString(sum[:]), checksum)
}

func TestDecodeEmailV1RejectsNoncanonicalAndUnknownShapes(t *testing.T) {
	encoded, _, err := MarshalEmailV1(unavailableEmail())
	require.NoError(t, err)
	for _, raw := range [][]byte{append(append([]byte{}, encoded...), '\n'), []byte(strings.Replace(string(encoded), `"inventory":null`, `"inventory":null,"unexpected":1`, 1)), []byte(strings.Replace(string(encoded), `"failure":{`, `"failure":{"code":"source_size_limit","code":"source_size_limit",`, 1))} {
		_, _, err = DecodeEmailV1(raw)
		require.Error(t, err)
	}
}

func TestEmailIdentityUsesLengthPrefixedTuple(t *testing.T) {
	value := unavailableEmail()
	_, checksum, err := MarshalEmailV1(value)
	require.NoError(t, err)
	generation, err := EmailGenerationID(value, checksum)
	require.NoError(t, err)
	recipe, err := EmailRecipeFingerprint(value.Recipe)
	require.NoError(t, err)
	expected := independentTupleHash("docbank-email-generation/v1", EmailContractV1, value.Source.SHA256, strconv.FormatInt(value.Source.Size, 10), recipe, checksum)
	assert.Equal(t, expected, generation)
	version := "123e4567-e89b-42d3-a456-426614174000"
	attachment, err := EmailAttachmentID(version, generation)
	require.NoError(t, err)
	assert.Equal(t, independentTupleHash("docbank-email-attachment/v1", version, generation), attachment)
	_, err = EmailGenerationID(value, strings.Repeat("0", 64))
	require.Error(t, err)
}

func TestEmailArtifactRolesAreClosedAndStable(t *testing.T) {
	assert.Equal(t, [3]EmailArtifactRole{
		EmailArtifactRawHeaders,
		EmailArtifactDecodedPayload,
		EmailArtifactBodyUTF8,
	}, EmailArtifactRoles())
	for _, role := range EmailArtifactRoles() {
		assert.True(t, IsEmailArtifactRole(string(role)))
	}
	assert.False(t, IsEmailArtifactRole("future_role"))
}

func independentTupleHash(values ...string) string {
	h := sha256.New()
	for _, value := range values {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(value))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestEmailBodyProfileIsCanonicalAndRecipeBound(t *testing.T) {
	profile, err := EmailBodyProfileV1(testEmailRecipe())
	require.NoError(t, err)
	_, fingerprints, err := CanonicalProfile(profile)
	require.NoError(t, err)
	assert.NotEmpty(t, fingerprints.Profile)
	changed := testEmailRecipe()
	changed.GoVersion = "go1.27.1"
	other, err := EmailBodyProfileV1(changed)
	require.NoError(t, err)
	_, otherFingerprints, err := CanonicalProfile(other)
	require.NoError(t, err)
	assert.NotEqual(t, fingerprints.Profile, otherFingerprints.Profile)
	changed = testEmailRecipe()
	changed.ImplementationRevision++
	other, err = EmailBodyProfileV1(changed)
	require.NoError(t, err)
	_, otherFingerprints, err = CanonicalProfile(other)
	require.NoError(t, err)
	assert.NotEqual(t, fingerprints.Profile, otherFingerprints.Profile)
}

func validDecodedEmail() EmailV1 {
	hash := strings.Repeat("0", 64)
	root := "1"
	media := "application/octet-stream"
	return EmailV1{ContractVersion: EmailContractV1, Source: EmailSourceV1{SHA256: strings.Repeat("a", 64), Size: 2, Verification: EmailVerificationVerified}, Recipe: testEmailRecipe(), Outcome: EmailOutcomeDecoded, Inventory: &EmailInventoryV1{State: EmailInventoryComplete, RootPath: &root, Parts: []EmailPartV1{{Path: "1", SiblingOrder: 1, Depth: 1, MessagePath: "1", Media: EmailMediaV1{Declared: &media, Diagnostics: []EmailDiagnosticV1{}}, HeaderBlock: &EmailArtifactRefV1{Role: EmailArtifactRawHeaders, SHA256: hash, Size: 2}, Headers: []EmailHeaderV1{}, Filename: EmailFilenameV1{Fields: []int{}, State: EmailInterpretationMissing}, ContentID: EmailContentIDV1{Fields: []int{}, State: EmailInterpretationMissing}, DecodeState: EmailDecodeDecoded, Protection: EmailProtectionNone, Payload: &EmailArtifactRefV1{Role: EmailArtifactDecodedPayload, SHA256: hash, Size: 0}, Diagnostics: []EmailDiagnosticV1{}}}, Messages: []EmailMessageV1{{Path: "1", Fields: EmailFieldsV1{Subject: []EmailDecodedFieldV1{}, From: []EmailDecodedFieldV1{}, To: []EmailDecodedFieldV1{}, Cc: []EmailDecodedFieldV1{}, Bcc: []EmailDecodedFieldV1{}, ReplyTo: []EmailDecodedFieldV1{}, MessageID: []EmailDecodedFieldV1{}, InReplyTo: []EmailDecodedFieldV1{}, References: []EmailDecodedFieldV1{}}, Date: EmailDateV1{State: EmailDateMissing, Fields: []int{}, TimezoneState: EmailTimezoneMissing, Diagnostics: []EmailDiagnosticV1{}}, Alternatives: []EmailAlternativeV1{}, RelatedGroups: []EmailRelatedGroupV1{}, Diagnostics: []EmailDiagnosticV1{}}}, Diagnostics: []EmailDiagnosticV1{}}}
}

func TestMarshalEmailV1RejectsNullListsUnsafeIntegersAndGraphDrift(t *testing.T) {
	for name, mutate := range map[string]func(*EmailV1){
		"null parts":        func(v *EmailV1) { v.Inventory.Parts = nil },
		"wrong role":        func(v *EmailV1) { v.Inventory.Parts[0].Payload.Role = EmailArtifactBodyUTF8 },
		"wrong owner":       func(v *EmailV1) { v.Inventory.Parts[0].MessagePath = "2" },
		"noncanonical path": func(v *EmailV1) { v.Inventory.Parts[0].Path = "01" },
		"unknown enum":      func(v *EmailV1) { v.Inventory.Parts[0].DecodeState = "maybe" },
	} {
		t.Run(name, func(t *testing.T) {
			value := validDecodedEmail()
			mutate(&value)
			_, _, err := MarshalEmailV1(value)
			require.Error(t, err)
		})
	}
	base := validDecodedEmail()
	encoded, _, err := MarshalEmailV1(base)
	require.NoError(t, err)
	unsafeInteger := strings.Replace(string(encoded), `"size":2`, `"size":9007199254740992`, 1)
	_, _, err = DecodeEmailV1([]byte(unsafeInteger))
	require.Error(t, err)
}

func TestValidateEmailPartPathBoundaries(t *testing.T) {
	for _, value := range []string{"1", "1.2.10", strings.Repeat("1.", 15) + "1"} {
		require.NoError(t, ValidateEmailPartPath(value))
	}
	for _, value := range []string{"", "0", "01", "1.0", "1.", strings.Repeat("1.", 16) + "1"} {
		require.Error(t, ValidateEmailPartPath(value))
	}
}

func TestMarshalEmailV1RejectsInvalidOrInconsistentParsedDates(t *testing.T) {
	base := validDecodedEmail()
	name := "date"
	base.Inventory.Parts[0].HeaderBlock.Size = 1
	base.Inventory.Parts[0].Headers = []EmailHeaderV1{{Index: 0, Offset: 0, Length: 1, Name: &name, State: EmailHeaderValid}}
	civil := "2026-09-09T12:34:56"
	zone := "EDT"
	utc := "2026-09-09T16:34:56Z"
	base.Inventory.Messages[0].Date = EmailDateV1{State: EmailDateParsed, Fields: []int{0}, Civil: &civil, UTC: &utc, TimezoneText: &zone, TimezoneState: EmailTimezoneKnownNamed, Diagnostics: []EmailDiagnosticV1{}}
	encoded, _, err := MarshalEmailV1(base)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*EmailV1){
		"invalid calendar":   func(value *EmailV1) { bad := "2026-09-31T12:34:56"; value.Inventory.Messages[0].Date.Civil = &bad },
		"wrong UTC":          func(value *EmailV1) { bad := "2026-09-09T12:34:56Z"; value.Inventory.Messages[0].Date.UTC = &bad },
		"unknown known zone": func(value *EmailV1) { bad := "XYZ"; value.Inventory.Messages[0].Date.TimezoneText = &bad },
	} {
		t.Run(name, func(t *testing.T) {
			value, _, decodeErr := DecodeEmailV1(encoded)
			require.NoError(t, decodeErr)
			mutate(&value)
			_, _, err := MarshalEmailV1(value)
			require.Error(t, err)
		})
	}
}

func TestMarshalEmailV1RejectsImpossibleParentsAndIncompleteAlternatives(t *testing.T) {
	t.Run("skipped immediate parent and child of leaf", func(t *testing.T) {
		value := validDecodedEmail()
		child := value.Inventory.Parts[0]
		parent := "1"
		child.Path = "1.1.1"
		child.ParentPath = &parent
		child.Depth = 2
		child.MessagePath = "1"
		value.Inventory.Parts = append(value.Inventory.Parts, child)
		_, _, err := MarshalEmailV1(value)
		require.Error(t, err)
	})
	t.Run("available body omitted from alternatives", func(t *testing.T) {
		value := validDecodedEmail()
		media := "text/plain"
		value.Inventory.Parts[0].Media.Declared = &media
		body := EmailArtifactRefV1{Role: EmailArtifactBodyUTF8, SHA256: strings.Repeat("1", 64), Size: 4}
		value.Inventory.Parts[0].BodyUTF8 = &body
		selected := "1"
		value.Inventory.Messages[0].SelectedBodyPath = &selected
		value.Inventory.Messages[0].Alternatives = []EmailAlternativeV1{{PartPath: "1", Kind: EmailBodyPlain, DisplayState: EmailDisplayAvailable, Display: &body, Diagnostics: []EmailDiagnosticV1{}}}
		_, _, err := MarshalEmailV1(value)
		require.NoError(t, err)
		value.Inventory.Messages[0].SelectedBodyPath = nil
		value.Inventory.Messages[0].Alternatives = []EmailAlternativeV1{}
		_, _, err = MarshalEmailV1(value)
		require.Error(t, err)
	})
	t.Run("attachment and encrypted ancestry cannot claim alternatives", func(t *testing.T) {
		value := validDecodedEmail()
		media := "text/plain"
		value.Inventory.Parts[0].Media.Declared = &media
		disposition := "attachment"
		value.Inventory.Parts[0].Disposition = &disposition
		value.Inventory.Messages[0].Alternatives = []EmailAlternativeV1{{PartPath: "1", Kind: EmailBodyPlain, DisplayState: EmailDisplayFailed, Diagnostics: []EmailDiagnosticV1{}}}
		_, _, err := MarshalEmailV1(value)
		require.Error(t, err)

		value = validDecodedEmail()
		encrypted := "multipart/encrypted"
		value.Inventory.Parts[0].Media.Declared = &encrypted
		value.Inventory.Parts[0].Protection = EmailProtectionEncrypted
		child := value.Inventory.Parts[0]
		parent := "1"
		child.Path = "1.1"
		child.ParentPath = &parent
		child.Depth = 2
		child.Media.Declared = &media
		child.Protection = EmailProtectionNone
		body := EmailArtifactRefV1{Role: EmailArtifactBodyUTF8, SHA256: strings.Repeat("1", 64), Size: 4}
		child.BodyUTF8 = &body
		value.Inventory.Parts = append(value.Inventory.Parts, child)
		value.Inventory.Messages[0].Alternatives = []EmailAlternativeV1{{PartPath: "1.1", Kind: EmailBodyPlain, DisplayState: EmailDisplayAvailable, Display: &body, Diagnostics: []EmailDiagnosticV1{}}}
		selected := "1.1"
		value.Inventory.Messages[0].SelectedBodyPath = &selected
		_, _, err = MarshalEmailV1(value)
		require.Error(t, err)
	})
}

func TestMarshalEmailV1RejectsDecodedFieldBoundToWrongHeader(t *testing.T) {
	value := validDecodedEmail()
	name := "subject"
	value.Inventory.Parts[0].HeaderBlock.Size = 1
	value.Inventory.Parts[0].Headers = []EmailHeaderV1{{Index: 0, Offset: 0, Length: 1, Name: &name, State: EmailHeaderValid}}
	text := "hello"
	value.Inventory.Messages[0].Fields.Subject = []EmailDecodedFieldV1{{HeaderIndex: 0, State: EmailInterpretationDecoded, Text: &text}}
	_, _, err := MarshalEmailV1(value)
	require.NoError(t, err)
	value.Inventory.Messages[0].Fields.From = value.Inventory.Messages[0].Fields.Subject
	_, _, err = MarshalEmailV1(value)
	require.Error(t, err)
}

func TestEmailInventorySizeUsesExactCanonicalBoundary(t *testing.T) {
	value := validDecodedEmail()
	name := "subject"
	value.Inventory.Parts[0].HeaderBlock.Size = 1
	value.Inventory.Parts[0].Headers = []EmailHeaderV1{{Index: 0, Offset: 0, Length: 1, Name: &name, State: EmailHeaderValid}}
	text := "control\nUnicode é"
	value.Inventory.Messages[0].Fields.Subject = []EmailDecodedFieldV1{{HeaderIndex: 0, State: EmailInterpretationDecoded, Text: &text}}
	inventory, err := canonical.Marshal(value.Inventory)
	require.NoError(t, err)
	for _, delta := range []int64{-1, 0, 1} {
		candidate := value
		candidate.Recipe.Limits.InventoryBytes = int64(len(inventory)) + delta
		observed, exceeded, err := emailInventorySize(candidate.Inventory, candidate.Recipe.Limits.InventoryBytes)
		require.NoError(t, err)
		assert.Equal(t, delta < 0, exceeded)
		if exceeded {
			assert.Equal(t, candidate.Recipe.Limits.InventoryBytes+1, observed)
		} else {
			assert.Equal(t, int64(len(inventory)), observed)
		}
	}
}
