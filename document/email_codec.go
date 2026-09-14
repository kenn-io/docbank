package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

const maxSafeJSONInteger = int64(1<<53 - 1)

var emailPartPathPattern = regexp.MustCompile(`^[1-9][0-9]*(\.[1-9][0-9]*){0,15}$`)
var emailUUIDv4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var emailCivilPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}$`)
var emailUTCPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
var emailNamedZoneOffsets = map[string]int{"UT": 0, "GMT": 0, "EST": -5 * 60, "EDT": -4 * 60, "CST": -6 * 60, "CDT": -5 * 60, "MST": -7 * 60, "MDT": -6 * 60, "PST": -8 * 60, "PDT": -7 * 60}

func MarshalEmailV1(value EmailV1) ([]byte, string, error) {
	if err := validateEmailV1(value); err != nil {
		return nil, "", err
	}
	if value.Inventory != nil {
		_, exceeded, sizeErr := emailInventorySize(value.Inventory, value.Recipe.Limits.InventoryBytes)
		if sizeErr != nil {
			return nil, "", fmt.Errorf("encoding email inventory: %w", sizeErr)
		}
		if exceeded {
			return nil, "", errors.New("email inventory metadata exceeds its limit")
		}
	}
	encoded, err := canonical.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("encoding email evidence: %w", err)
	}
	return encoded, emailSHA256(encoded), nil
}

func emailInventorySize(value *EmailInventoryV1, limit int64) (observed int64, exceeded bool, err error) {
	return canonical.BoundedSize(value, limit)
}

func DecodeEmailV1(encoded []byte) (EmailV1, string, error) {
	if int64(len(encoded)) > (8<<20)+(64<<10) {
		return EmailV1{}, "", errors.New("email evidence exceeds the canonical inventory limit")
	}
	value, err := canonical.Decode[EmailV1](encoded)
	if err != nil {
		return EmailV1{}, "", fmt.Errorf("decoding email evidence: %w", err)
	}
	reencoded, checksum, err := MarshalEmailV1(value)
	if err != nil {
		return EmailV1{}, "", err
	}
	if !bytes.Equal(encoded, reencoded) {
		return EmailV1{}, "", errors.New("email evidence bytes are not canonical")
	}
	return value, checksum, nil
}

func EmailRecipeFingerprint(recipe EmailRecipeV1) (string, error) {
	if err := validateEmailRecipe(recipe); err != nil {
		return "", err
	}
	encoded, err := canonical.Marshal(recipe)
	if err != nil {
		return "", fmt.Errorf("encoding email recipe: %w", err)
	}
	return emailSHA256(encoded), nil
}

func EmailBodyRecipeFingerprint(recipe EmailRecipeV1) (string, error) {
	fingerprint, err := EmailRecipeFingerprint(recipe)
	if err != nil {
		return "", err
	}
	return emailTupleHash("docbank-email-body-recipe/v1", fingerprint,
		"docbank-email-structural-text/v1", "docbank-email-literal-units/v1"), nil
}

func EmailGenerationID(value EmailV1, checksum string) (string, error) {
	_, actual, err := MarshalEmailV1(value)
	if err != nil {
		return "", err
	}
	if !canonical.IsSHA256Hex(checksum) || checksum != actual {
		return "", errors.New("email representation checksum is invalid")
	}
	recipe, err := EmailRecipeFingerprint(value.Recipe)
	if err != nil {
		return "", err
	}
	return emailTupleHash("docbank-email-generation/v1", value.ContractVersion, value.Source.SHA256,
		strconv.FormatInt(value.Source.Size, 10), recipe, checksum), nil
}

func EmailAttachmentID(versionID, generationID string) (string, error) {
	if !emailUUIDv4Pattern.MatchString(versionID) {
		return "", errors.New("email content version ID is invalid")
	}
	if !canonical.IsSHA256Hex(generationID) {
		return "", errors.New("email generation ID is invalid")
	}
	return emailTupleHash("docbank-email-attachment/v1", versionID, generationID), nil
}

func ValidateEmailPartPath(path string) error {
	if !utf8.ValidString(path) || !emailPartPathPattern.MatchString(path) {
		return errors.New("email part path is invalid")
	}
	for component := range strings.SplitSeq(path, ".") {
		if len(component) > 9 {
			return errors.New("email part path component is too large")
		}
		value, err := strconv.Atoi(component)
		if err != nil || value < 1 || value > 1_000_000_000 {
			return errors.New("email part path component is invalid")
		}
	}
	return nil
}

func emailTupleHash(values ...string) string {
	h := sha256.New()
	var size [8]byte
	for _, value := range values {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(value))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func emailSHA256(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }

func validateEmailRecipe(recipe EmailRecipeV1) error {
	if recipe.ContractVersion != EmailRecipeContractV1 {
		return fmt.Errorf("email recipe contract version must be %q", EmailRecipeContractV1)
	}
	if recipe.ImplementationRevision < 1 {
		return errors.New("email recipe implementation revision must be positive")
	}
	if recipe.GoVersion == "" || len(recipe.GoVersion) > 128 || !utf8.ValidString(recipe.GoVersion) {
		return errors.New("email recipe Go version is invalid")
	}
	if recipe.CharsetProfile != "docbank-email-charset/v1" || recipe.HeaderProfile != "docbank-email-header/v1" ||
		recipe.FilenameProfile != "docbank-email-filename/v1" || recipe.BodyProfile != "docbank-email-body-selection/v1" {
		return errors.New("email recipe profile is invalid")
	}
	want := canonicalEmailLimits()
	if recipe.Limits != want {
		return errors.New("email recipe limits are invalid")
	}
	return nil
}

func canonicalEmailLimits() EmailLimitsV1 {
	return EmailLimitsV1{SourceBytes: 128 << 20, PartBytes: 128 << 20, DecodedBytes: 256 << 20,
		Parts: 1000, Depth: 16, HeaderBytes: 1 << 20, AggregateHeaderBytes: 8 << 20,
		HeaderFields: 4096, AggregateHeaderFields: 16384, BodyUTF8Bytes: 16 << 20,
		AggregateBodyUTF8Bytes: 256 << 20, HTMLDisplayBytes: 16 << 20, InventoryBytes: 8 << 20,
		Diagnostics: 4096, DiagnosticDetailBytes: 1024, HeaderDisplayBytes: 1 << 20}
}

func validateEmailV1(value EmailV1) error {
	if value.ContractVersion != EmailContractV1 {
		return fmt.Errorf("email contract version must be %q", EmailContractV1)
	}
	if !canonical.IsSHA256Hex(value.Source.SHA256) {
		return errors.New("email source SHA-256 is invalid")
	}
	if value.Source.Size < 0 || value.Source.Size > maxSafeJSONInteger {
		return errors.New("email source size is invalid")
	}
	if err := validateEmailRecipe(value.Recipe); err != nil {
		return err
	}
	switch value.Outcome {
	case EmailOutcomeDecoded:
		if value.Source.Verification != EmailVerificationVerified || value.Inventory == nil || value.Failure != nil {
			return errors.New("decoded email requires verified source, inventory, and no failure")
		}
		if value.Source.Size > value.Recipe.Limits.SourceBytes {
			return errors.New("decoded email source exceeds its recipe limit")
		}
		return validateEmailInventory(value.Inventory, value.Recipe.Limits)
	case EmailOutcomeUnavailable:
		if value.Inventory != nil || value.Failure == nil {
			return errors.New("unavailable email requires failure and no inventory")
		}
		if value.Source.Verification != EmailVerificationVerified && value.Source.Verification != EmailVerificationCatalogOnly {
			return errors.New("unavailable email source verification is invalid")
		}
		if err := validateEmailFailure(*value.Failure, value.Recipe.Limits, true); err != nil {
			return err
		}
		if value.Failure.Code == EmailDiagnosticSourceSizeLimit {
			if value.Source.Verification != EmailVerificationCatalogOnly {
				return errors.New("source-size refusal must be catalog only")
			}
			if value.Source.Size <= value.Recipe.Limits.SourceBytes || value.Failure.Limit == nil || *value.Failure.Limit != value.Recipe.Limits.SourceBytes || value.Failure.Observed == nil || *value.Failure.Observed != value.Source.Size {
				return errors.New("source-size refusal bounds are invalid")
			}
		} else if value.Source.Verification != EmailVerificationVerified {
			return errors.New("non-size refusal requires verified source")
		}
		return nil
	default:
		return errors.New("email outcome is invalid")
	}
}

func validateEmailInventory(inventory *EmailInventoryV1, limits EmailLimitsV1) error {
	if inventory.Parts == nil || inventory.Messages == nil || inventory.Diagnostics == nil {
		return errors.New("email inventory lists must not be null")
	}
	if len(inventory.Parts) > limits.Parts {
		return errors.New("email inventory has too many parts")
	}
	switch inventory.State {
	case EmailInventoryComplete:
		if inventory.RootPath == nil || *inventory.RootPath != "1" || inventory.Termination != nil || len(inventory.Parts) == 0 {
			return errors.New("complete email inventory has invalid root or termination")
		}
	case EmailInventoryPartial:
		if inventory.Termination == nil {
			return errors.New("partial email inventory requires termination")
		}
		if err := validateEmailTermination(*inventory.Termination, limits); err != nil {
			return err
		}
		if len(inventory.Parts) == 0 {
			if inventory.RootPath != nil {
				return errors.New("empty partial inventory must have null root")
			}
		} else if inventory.RootPath == nil || *inventory.RootPath != "1" {
			return errors.New("observed partial inventory must have root 1")
		}
	default:
		return errors.New("email inventory state is invalid")
	}
	parts := make(map[string]EmailPartV1, len(inventory.Parts))
	nextSibling := make(map[string]int)
	diagnostics := len(inventory.Diagnostics)
	var headerBytes int64
	var headerFields int
	var decodedBytes int64
	var bodyUTF8Bytes int64
	var headerDisplayBytes int64
	for index, part := range inventory.Parts {
		if index > 0 && compareEmailPartPaths(inventory.Parts[index-1].Path, part.Path) >= 0 {
			return errors.New("email parts are not in depth-first numeric order")
		}
		if err := validateEmailPart(part, inventory, parts, nextSibling, limits); err != nil {
			return fmt.Errorf("email part %d: %w", index, err)
		}
		nextSibling[parentKey(part.ParentPath)] = part.SiblingOrder + 1
		if part.HeaderBlock != nil {
			headerBytes += part.HeaderBlock.Size
		}
		if part.Payload != nil {
			decodedBytes += part.Payload.Size
		}
		if part.BodyUTF8 != nil {
			bodyUTF8Bytes += part.BodyUTF8.Size
		}
		if part.Filename.Decoded != nil {
			headerDisplayBytes += int64(len(*part.Filename.Decoded))
		}
		if part.ContentID.Value != nil {
			headerDisplayBytes += int64(len(*part.ContentID.Value))
		}
		headerFields += len(part.Headers)
		diagnostics += len(part.Diagnostics) + len(part.Media.Diagnostics)
	}
	if headerBytes > limits.AggregateHeaderBytes || headerFields > limits.AggregateHeaderFields {
		return errors.New("email aggregate header limits exceeded")
	}
	if decodedBytes > limits.DecodedBytes {
		return errors.New("email aggregate decoded payload limit exceeded")
	}
	if bodyUTF8Bytes > limits.AggregateBodyUTF8Bytes {
		return errors.New("email aggregate UTF-8 body limit exceeded")
	}
	for _, part := range inventory.Parts {
		if part.BodyUTF8 != nil && !emailBodyEligible(part, parts) {
			return errors.New("ineligible email body owns a UTF-8 derivative")
		}
	}
	messages := make(map[string]bool, len(inventory.Messages))
	diagnosticsComplete := inventory.Termination == nil || inventory.Termination.Code != EmailDiagnosticLimit
	expectedMessagePaths := make([]string, 0)
	for _, part := range inventory.Parts {
		if part.ParentPath == nil {
			expectedMessagePaths = append(expectedMessagePaths, part.Path)
			continue
		}
		parent := parts[*part.ParentPath]
		if parent.Media.Declared != nil && *parent.Media.Declared == "message/rfc822" {
			expectedMessagePaths = append(expectedMessagePaths, part.Path)
		}
	}
	if len(inventory.Messages) != len(expectedMessagePaths) {
		return errors.New("email messages do not match message roots")
	}
	for index, message := range inventory.Messages {
		if message.Path != expectedMessagePaths[index] {
			return fmt.Errorf("email message %d is out of structural order", index)
		}
		if messages[message.Path] {
			return fmt.Errorf("email message %d duplicates path", index)
		}
		messages[message.Path] = true
		if err := validateEmailMessage(message, parts, inventory.Parts, limits, diagnosticsComplete); err != nil {
			return fmt.Errorf("email message %d: %w", index, err)
		}
		headerDisplayBytes += emailMessageDisplayBytes(message)
		diagnostics += len(message.Diagnostics) + len(message.Date.Diagnostics)
		for _, alternative := range message.Alternatives {
			diagnostics += len(alternative.Diagnostics)
		}
	}
	for _, part := range inventory.Parts {
		if !messages[part.MessagePath] {
			return fmt.Errorf("email part %q has unknown message owner", part.Path)
		}
	}
	if diagnostics > limits.Diagnostics {
		return errors.New("email diagnostics exceed their limit")
	}
	if headerDisplayBytes > limits.HeaderDisplayBytes {
		return errors.New("email aggregate header display limit exceeded")
	}
	for _, diagnostic := range inventory.Diagnostics {
		if err := validateEmailDiagnostic(diagnostic, parts, limits); err != nil {
			return err
		}
	}
	return nil
}

func parentKey(path *string) string {
	if path == nil {
		return ""
	}
	return *path
}

func validateEmailPart(part EmailPartV1, inventory *EmailInventoryV1, prior map[string]EmailPartV1, nextSibling map[string]int, limits EmailLimitsV1) error {
	if err := ValidateEmailPartPath(part.Path); err != nil {
		return err
	}
	if _, exists := prior[part.Path]; exists {
		return errors.New("duplicate email part path")
	}
	if part.Headers == nil || part.Diagnostics == nil || part.Media.Diagnostics == nil || part.Filename.Fields == nil || part.ContentID.Fields == nil {
		return errors.New("email part lists must not be null")
	}
	if part.Depth < 1 || part.Depth > limits.Depth || part.SiblingOrder < 1 {
		return errors.New("email part depth or sibling order is invalid")
	}
	if part.ParentPath == nil {
		if part.Path != "1" || part.Depth != 1 || part.SiblingOrder != 1 || len(prior) != 0 {
			return errors.New("email root part is invalid")
		}
	} else {
		parent, ok := prior[*part.ParentPath]
		lastDot := strings.LastIndexByte(part.Path, '.')
		if !ok || lastDot < 0 || part.Path[:lastDot] != parent.Path || part.Depth != parent.Depth+1 {
			return errors.New("email part parent is invalid")
		}
		parentMedia := "text/plain"
		if parent.Media.Declared != nil {
			parentMedia = *parent.Media.Declared
		}
		switch {
		case strings.HasPrefix(parentMedia, "multipart/"):
			if parent.DecodeState != EmailDecodeDecoded || part.MessagePath != parent.MessagePath {
				return errors.New("multipart child changed message owner")
			}
		case parentMedia == "message/rfc822":
			if parent.DecodeState != EmailDecodeDecoded || part.SiblingOrder != 1 || part.MessagePath != part.Path {
				return errors.New("enclosed message root is invalid")
			}
		default:
			return errors.New("email leaf part cannot have children")
		}
		last := part.Path[strings.LastIndexByte(part.Path, '.')+1:]
		order, _ := strconv.Atoi(last)
		if order != part.SiblingOrder {
			return errors.New("email part path and sibling order disagree")
		}
	}
	wantSibling := nextSibling[parentKey(part.ParentPath)]
	if wantSibling == 0 {
		wantSibling = 1
	}
	if part.SiblingOrder != wantSibling {
		return errors.New("email sibling order is not contiguous")
	}
	if err := ValidateEmailPartPath(part.MessagePath); err != nil {
		return errors.New("email message owner path is invalid")
	}
	if part.Path != part.MessagePath && !strings.HasPrefix(part.Path, part.MessagePath+".") {
		return errors.New("email message owner is not an ancestor")
	}
	if part.HeaderBlock == nil {
		if inventory.State != EmailInventoryPartial || inventory.Termination == nil || inventory.Termination.Path == nil || *inventory.Termination.Path != part.Path || inventory.Termination.Operation != EmailOperationHeaders {
			return errors.New("email part lacks unexplained header block")
		}
	} else if err := validateArtifact(*part.HeaderBlock, EmailArtifactRawHeaders, limits.HeaderBytes); err != nil {
		return err
	}
	if len(part.Headers) > limits.HeaderFields {
		return errors.New("email part has too many headers")
	}
	var end int64
	for index, header := range part.Headers {
		if header.Index != index || header.Offset != end || header.Length <= 0 || part.HeaderBlock == nil || header.Offset+header.Length > part.HeaderBlock.Size {
			return errors.New("email header span is invalid")
		}
		end = header.Offset + header.Length
		switch header.State {
		case EmailHeaderValid:
			if header.Name == nil || *header.Name == "" || *header.Name != strings.ToLower(*header.Name) || !validEmailHeaderName(*header.Name) {
				return errors.New("valid email header requires a name")
			}
		case EmailHeaderMalformed:
			if header.Name != nil {
				return errors.New("malformed email header cannot have a name")
			}
		default:
			return errors.New("email header state is invalid")
		}
	}
	if part.Disposition != nil && (*part.Disposition == "" || *part.Disposition != strings.ToLower(*part.Disposition)) {
		return errors.New("email disposition is invalid")
	}
	if part.Media.Declared != nil && !validEmailMediaType(*part.Media.Declared) {
		return errors.New("declared email media type is invalid")
	}
	if part.Media.Detected != nil && !validEmailMediaType(*part.Media.Detected) {
		return errors.New("detected email media type is invalid")
	}
	if part.TransferEncoding != nil && (*part.TransferEncoding == "" || *part.TransferEncoding != strings.ToLower(*part.TransferEncoding)) {
		return errors.New("email transfer encoding is invalid")
	}
	if err := validateFilename(part.Filename, len(part.Headers), part.Path); err != nil {
		return err
	}
	for _, index := range part.Filename.Fields {
		header := part.Headers[index]
		if header.Name == nil || *header.Name != "content-disposition" && *header.Name != "content-type" {
			return errors.New("email filename references a header with the wrong name")
		}
	}
	if err := validateContentID(part.ContentID, len(part.Headers)); err != nil {
		return err
	}
	if !slices.Equal(part.ContentID.Fields, emailHeaderIndexesByName(part.Headers, "content-id")) {
		return errors.New("email content ID interpretations do not match headers")
	}
	switch part.DecodeState {
	case EmailDecodeDecoded:
		if part.Payload == nil {
			return errors.New("decoded email part requires payload")
		}
		if err := validateArtifact(*part.Payload, EmailArtifactDecodedPayload, limits.PartBytes); err != nil {
			return err
		}
	case EmailDecodeUnsupported, EmailDecodeFailed:
		if part.Payload != nil {
			return errors.New("non-decoded email part cannot have payload")
		}
	default:
		return errors.New("email decode state is invalid")
	}
	if part.BodyUTF8 != nil {
		if part.DecodeState != EmailDecodeDecoded {
			return errors.New("body UTF-8 requires decoded payload")
		}
		if err := validateArtifact(*part.BodyUTF8, EmailArtifactBodyUTF8, limits.BodyUTF8Bytes); err != nil {
			return err
		}
	}
	if part.Protection != EmailProtectionNone && part.Protection != EmailProtectionSignedUnverified && part.Protection != EmailProtectionEncrypted {
		return errors.New("email protection state is invalid")
	}
	declared := ""
	if part.Media.Declared != nil {
		declared = *part.Media.Declared
	}
	wantProtection := EmailProtectionNone
	switch declared {
	case "multipart/signed":
		wantProtection = EmailProtectionSignedUnverified
	case "multipart/encrypted", "application/pkcs7-mime", "application/x-pkcs7-mime":
		wantProtection = EmailProtectionEncrypted
	}
	if part.Protection != wantProtection {
		return errors.New("email protection does not match declared media")
	}
	prior[part.Path] = part
	for _, diagnostic := range part.Diagnostics {
		if err := validateEmailDiagnostic(diagnostic, prior, limits); err != nil {
			return err
		}
	}
	for _, diagnostic := range part.Media.Diagnostics {
		if err := validateEmailDiagnostic(diagnostic, prior, limits); err != nil {
			return err
		}
	}
	return nil
}

func validateArtifact(ref EmailArtifactRefV1, role EmailArtifactRole, limit int64) error {
	if ref.Role != role || !canonical.IsSHA256Hex(ref.SHA256) || ref.Size < 0 || ref.Size > limit {
		return fmt.Errorf("email %s artifact reference is invalid", role)
	}
	return nil
}

func validateFilename(value EmailFilenameV1, headers int, partPath string) error {
	if !validHeaderIndexes(value.Fields, headers) {
		return errors.New("email filename header references are invalid")
	}
	switch value.State {
	case EmailInterpretationMissing:
		if len(value.Fields) != 0 || value.Decoded != nil || value.SafeName != "" {
			return errors.New("missing email filename is invalid")
		}
	case EmailInterpretationDecoded:
		if value.Decoded == nil || value.SafeName == "" || !utf8.ValidString(*value.Decoded) {
			return errors.New("decoded email filename is invalid")
		}
		safe, err := SafeEmailFilename(*value.Decoded, partPath)
		if err != nil || safe != value.SafeName {
			return errors.New("email safe filename does not match its decoded value")
		}
	case EmailInterpretationInvalid, EmailInterpretationUnsupported:
		if value.Decoded != nil || value.SafeName != "" || len(value.Fields) == 0 {
			return errors.New("unavailable email filename is invalid")
		}
	default:
		return errors.New("email filename state is invalid")
	}
	return nil
}

func validateContentID(value EmailContentIDV1, headers int) error {
	if !validHeaderIndexes(value.Fields, headers) {
		return errors.New("email content ID header references are invalid")
	}
	switch value.State {
	case EmailInterpretationMissing:
		if len(value.Fields) != 0 || value.Value != nil {
			return errors.New("missing email content ID is invalid")
		}
	case EmailInterpretationDecoded:
		if value.Value == nil || *value.Value == "" || strings.TrimSpace(*value.Value) != *value.Value {
			return errors.New("decoded email content ID is invalid")
		}
	case EmailInterpretationInvalid, EmailInterpretationUnsupported:
		if value.Value != nil || len(value.Fields) == 0 {
			return errors.New("unavailable email content ID is invalid")
		}
	default:
		return errors.New("email content ID state is invalid")
	}
	return nil
}

func validHeaderIndexes(indexes []int, count int) bool {
	last := -1
	for _, value := range indexes {
		if value < 0 || value >= count || value <= last {
			return false
		}
		last = value
	}
	return true
}

func validateEmailMessage(message EmailMessageV1, parts map[string]EmailPartV1, orderedParts []EmailPartV1, limits EmailLimitsV1, diagnosticsComplete bool) error {
	part, ok := parts[message.Path]
	if !ok || part.MessagePath != message.Path {
		return errors.New("email message path is not a message root")
	}
	if message.Alternatives == nil || message.RelatedGroups == nil || message.Diagnostics == nil || message.Date.Fields == nil || message.Date.Diagnostics == nil {
		return errors.New("email message lists must not be null")
	}
	fields := []struct {
		name      string
		addresses bool
		list      []EmailDecodedFieldV1
	}{
		{"subject", false, message.Fields.Subject}, {"from", true, message.Fields.From}, {"to", true, message.Fields.To},
		{"cc", true, message.Fields.Cc}, {"bcc", true, message.Fields.Bcc}, {"reply-to", true, message.Fields.ReplyTo},
		{"message-id", false, message.Fields.MessageID}, {"in-reply-to", false, message.Fields.InReplyTo}, {"references", false, message.Fields.References},
	}
	for _, definition := range fields {
		list := definition.list
		if list == nil {
			return errors.New("email decoded field list must not be null")
		}
		expected := emailHeaderIndexesByName(part.Headers, definition.name)
		if len(list) != len(expected) {
			return fmt.Errorf("email %s interpretations do not match headers", definition.name)
		}
		for index, field := range list {
			if field.HeaderIndex != expected[index] {
				return fmt.Errorf("email %s interpretation order is invalid", definition.name)
			}
			if err := validateDecodedField(field, len(part.Headers)); err != nil {
				return err
			}
			if !definition.addresses && field.Addresses != nil {
				return fmt.Errorf("email %s interpretation cannot contain addresses", definition.name)
			}
			if definition.addresses && field.State == EmailInterpretationDecoded && field.Addresses == nil {
				return fmt.Errorf("decoded email %s interpretation requires addresses", definition.name)
			}
		}
	}
	if !slices.Equal(message.Date.Fields, emailHeaderIndexesByName(part.Headers, "date")) {
		return errors.New("email date interpretations do not match headers")
	}
	if err := validateEmailDate(message.Date, len(part.Headers), parts, limits); err != nil {
		return err
	}
	selected := ""
	if message.SelectedBodyPath != nil {
		selected = *message.SelectedBodyPath
	}
	selectedFound := selected == ""
	wantSelected := ""
	var wantKind EmailBodyKind
	seenAlternatives := make(map[string]bool, len(message.Alternatives))
	expectedAlternatives := make([]EmailPartV1, 0)
	for _, candidate := range orderedParts {
		if candidate.MessagePath == message.Path && emailBodyEligible(candidate, parts) {
			expectedAlternatives = append(expectedAlternatives, candidate)
		}
	}
	if len(message.Alternatives) != len(expectedAlternatives) {
		return errors.New("email alternatives do not match eligible MIME bodies")
	}
	for alternativeIndex, alternative := range message.Alternatives {
		if alternative.PartPath != expectedAlternatives[alternativeIndex].Path {
			return errors.New("email alternatives are not in MIME part order")
		}
		if seenAlternatives[alternative.PartPath] {
			return errors.New("email alternative path is duplicated")
		}
		seenAlternatives[alternative.PartPath] = true
		candidate, ok := parts[alternative.PartPath]
		if !ok || candidate.MessagePath != message.Path {
			return errors.New("email alternative references wrong message")
		}
		if alternative.Kind != EmailBodyHTML && alternative.Kind != EmailBodyPlain {
			return errors.New("email alternative kind is invalid")
		}
		if alternative.Kind == EmailBodyHTML && (candidate.Media.Declared == nil || *candidate.Media.Declared != "text/html") {
			return errors.New("HTML email alternative media type is invalid")
		}
		if alternative.Kind == EmailBodyPlain && candidate.Media.Declared != nil && *candidate.Media.Declared != "text/plain" {
			return errors.New("plain email alternative media type is invalid")
		}
		if alternative.Diagnostics == nil {
			return errors.New("email alternative diagnostics must not be null")
		}
		for _, diagnostic := range alternative.Diagnostics {
			if err := validateEmailDiagnostic(diagnostic, parts, limits); err != nil {
				return err
			}
		}
		switch alternative.DisplayState {
		case EmailDisplayAvailable:
			if alternative.Display == nil || candidate.BodyUTF8 == nil || *alternative.Display != *candidate.BodyUTF8 {
				return errors.New("available email alternative display is invalid")
			}
			if wantSelected == "" || (alternative.Kind == EmailBodyHTML && wantKind != EmailBodyHTML) {
				wantSelected = alternative.PartPath
				wantKind = alternative.Kind
			}
		case EmailDisplayUnsupported, EmailDisplayFailed, EmailDisplayTooLarge:
			if alternative.Display != nil || candidate.BodyUTF8 != nil {
				return errors.New("unavailable email alternative cannot have display")
			}
		default:
			return errors.New("email alternative display state is invalid")
		}
		if alternative.PartPath == selected {
			if alternative.DisplayState != EmailDisplayAvailable {
				return errors.New("selected email body is unavailable")
			}
			selectedFound = true
		}
	}
	if !selectedFound {
		return errors.New("selected email body does not resolve")
	}
	if selected != wantSelected {
		return errors.New("selected email body does not follow HTML-then-plain policy")
	}
	if err := validateEmailRelatedGroups(message, parts, orderedParts, diagnosticsComplete); err != nil {
		return err
	}
	for _, diagnostic := range message.Diagnostics {
		if err := validateEmailDiagnostic(diagnostic, parts, limits); err != nil {
			return err
		}
	}
	return nil
}

func emailHeaderIndexesByName(headers []EmailHeaderV1, name string) []int {
	indexes := make([]int, 0)
	for _, header := range headers {
		if header.State == EmailHeaderValid && header.Name != nil && *header.Name == name {
			indexes = append(indexes, header.Index)
		}
	}
	return indexes
}

func emailBodyEligible(part EmailPartV1, parts map[string]EmailPartV1) bool {
	messagePath := part.MessagePath
	media := "text/plain"
	if part.Media.Declared != nil {
		media = *part.Media.Declared
	} else {
		for _, diagnostic := range part.Media.Diagnostics {
			if diagnostic.Code == EmailDiagnosticInvalidHeader {
				return false
			}
		}
	}
	if media != "text/plain" && media != "text/html" || part.IsAttachmentLike() {
		return false
	}
	current := part
	for current.ParentPath != nil {
		parent, ok := parts[*current.ParentPath]
		if !ok {
			return false
		}
		if parent.MessagePath != messagePath {
			break
		}
		if parent.Protection == EmailProtectionEncrypted || parent.IsAttachmentLike() {
			return false
		}
		current = parent
	}
	return true
}

func validateEmailRelatedGroups(message EmailMessageV1, parts map[string]EmailPartV1, orderedParts []EmailPartV1, diagnosticsComplete bool) error {
	children := make(map[string]bool, len(orderedParts))
	for _, part := range orderedParts {
		if part.ParentPath != nil {
			children[*part.ParentPath] = true
		}
	}
	alternatives := make(map[string]EmailAlternativeV1, len(message.Alternatives))
	for _, alternative := range message.Alternatives {
		alternatives[alternative.PartPath] = alternative
	}
	want := make([]EmailRelatedGroupV1, 0)
	wantMissingDiagnostic := make(map[string]bool)
	for _, root := range orderedParts {
		if root.MessagePath != message.Path || root.Media.Declared == nil || *root.Media.Declared != "multipart/related" {
			continue
		}
		group := EmailRelatedGroupV1{RootPath: root.Path, Resources: []EmailResourceV1{}}
		var plain *string
		for _, alternative := range message.Alternatives {
			if alternative.DisplayState != EmailDisplayAvailable || nearestEmailRelatedRoot(alternative.PartPath, message.Path, parts) != root.Path {
				continue
			}
			path := alternative.PartPath
			if alternative.Kind == EmailBodyHTML && group.BodyPath == nil {
				group.BodyPath = &path
			} else if alternative.Kind == EmailBodyPlain && plain == nil {
				plain = &path
			}
		}
		if group.BodyPath == nil {
			group.BodyPath = plain
		}
		resourceIndex := make(map[string]int)
		missingAdded := false
		for _, part := range orderedParts {
			if part.Path == root.Path || part.MessagePath != message.Path || nearestEmailRelatedRoot(part.Path, message.Path, parts) != root.Path {
				continue
			}
			if part.ContentID.State == EmailInterpretationDecoded && part.ContentID.Value != nil {
				cid := *part.ContentID.Value
				index, ok := resourceIndex[cid]
				if !ok {
					value := cid
					resourceIndex[cid] = len(group.Resources)
					group.Resources = append(group.Resources, EmailResourceV1{CID: &value, Candidates: []string{part.Path}, State: EmailResourceUnique})
				} else {
					resource := &group.Resources[index]
					resource.Candidates = append(resource.Candidates, part.Path)
					resource.State = EmailResourceAmbiguous
				}
				continue
			}
			_, bodyAlternative := alternatives[part.Path]
			declared := ""
			if part.Media.Declared != nil {
				declared = *part.Media.Declared
			}
			eligible := !children[part.Path] && !bodyAlternative && !strings.HasPrefix(declared, "multipart/") && declared != "message/rfc822" && (part.Disposition == nil || *part.Disposition != "attachment")
			if eligible && !missingAdded {
				group.Resources = append(group.Resources, EmailResourceV1{CID: nil, Candidates: []string{}, State: EmailResourceMissing})
				missingAdded = true
			}
			if eligible {
				wantMissingDiagnostic[part.Path] = true
			}
		}
		want = append(want, group)
	}
	for _, part := range orderedParts {
		if part.MessagePath == message.Path {
			got, expected := countEmailCIDMissingDiagnostics(part), boolInt(wantMissingDiagnostic[part.Path])
			if (diagnosticsComplete && got != expected) || (!diagnosticsComplete && got > expected) {
				return errors.New("email related missing-ID diagnostics are invalid")
			}
		}
	}
	if len(message.RelatedGroups) != len(want) {
		return errors.New("email related groups do not match MIME structure")
	}
	for index := range want {
		got, expected := message.RelatedGroups[index], want[index]
		if got.RootPath != expected.RootPath || !equalOptionalString(got.BodyPath, expected.BodyPath) || got.Resources == nil || len(got.Resources) != len(expected.Resources) {
			return errors.New("email related group is invalid")
		}
		for resourceIndex := range expected.Resources {
			actualResource, expectedResource := got.Resources[resourceIndex], expected.Resources[resourceIndex]
			if actualResource.Candidates == nil || actualResource.State != expectedResource.State || !equalOptionalString(actualResource.CID, expectedResource.CID) || !slices.Equal(actualResource.Candidates, expectedResource.Candidates) {
				return errors.New("email related resource inventory is invalid")
			}
		}
	}
	return nil
}

func nearestEmailRelatedRoot(path, messagePath string, parts map[string]EmailPartV1) string {
	part, ok := parts[path]
	for ok && part.ParentPath != nil {
		parent, exists := parts[*part.ParentPath]
		if !exists || parent.MessagePath != messagePath {
			return ""
		}
		if parent.Media.Declared != nil && *parent.Media.Declared == "multipart/related" {
			return parent.Path
		}
		part, ok = parent, true
	}
	return ""
}

func countEmailCIDMissingDiagnostics(part EmailPartV1) int {
	count := 0
	for _, diagnostic := range part.Diagnostics {
		if diagnostic.Code == EmailDiagnosticCIDMissing && diagnostic.Operation == EmailOperationCID && diagnostic.Path != nil && *diagnostic.Path == part.Path && diagnostic.HeaderIndex == nil {
			count++
		}
	}
	return count
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func equalOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func validateDecodedField(field EmailDecodedFieldV1, headers int) error {
	if field.HeaderIndex < 0 || field.HeaderIndex >= headers {
		return errors.New("email decoded field header index is invalid")
	}
	if field.State == EmailInterpretationMissing {
		return errors.New("present email decoded field cannot be missing")
	}
	switch field.State {
	case EmailInterpretationDecoded:
		if field.Text == nil {
			return errors.New("decoded email field requires text")
		}
	case EmailInterpretationInvalid:
		if field.Text == nil && field.Addresses != nil {
			return errors.New("invalid email field without text cannot have addresses")
		}
	case EmailInterpretationUnsupported:
		if field.Text != nil || field.Addresses != nil {
			return errors.New("unsupported email field must be empty")
		}
	default:
		return errors.New("email decoded field state is invalid")
	}
	if field.Addresses != nil {
		for _, address := range *field.Addresses {
			if !utf8.ValidString(address.Name) || !utf8.ValidString(address.Address) || address.Address == "" {
				return errors.New("email address is invalid")
			}
		}
	}
	return nil
}

func validateEmailDate(date EmailDateV1, headers int, parts map[string]EmailPartV1, limits EmailLimitsV1) error {
	if !validHeaderIndexes(date.Fields, headers) {
		return errors.New("email date header references are invalid")
	}
	switch date.State {
	case EmailDateMissing:
		if len(date.Fields) != 0 || date.Civil != nil || date.UTC != nil || date.TimezoneText != nil || date.TimezoneState != EmailTimezoneMissing {
			return errors.New("missing email date is invalid")
		}
	case EmailDateInvalid:
		if len(date.Fields) == 0 || date.Civil != nil || date.UTC != nil || date.TimezoneState != EmailTimezoneInvalid {
			return errors.New("invalid email date is invalid")
		}
	case EmailDateParsed:
		if len(date.Fields) != 1 || date.Civil == nil || !emailCivilPattern.MatchString(*date.Civil) {
			return errors.New("parsed email date is invalid")
		}
		if date.TimezoneState == EmailTimezoneInvalid {
			return errors.New("parsed email date cannot have invalid timezone")
		}
	default:
		return errors.New("email date state is invalid")
	}
	if date.TimezoneState != EmailTimezoneMissing && date.TimezoneState != EmailTimezoneNumeric && date.TimezoneState != EmailTimezoneKnownNamed && date.TimezoneState != EmailTimezoneUnknownNamed && date.TimezoneState != EmailTimezoneInvalid {
		return errors.New("email timezone state is invalid")
	}
	if date.TimezoneState == EmailTimezoneMissing && date.TimezoneText != nil {
		return errors.New("missing email timezone cannot have text")
	}
	if date.State == EmailDateParsed && date.TimezoneState != EmailTimezoneMissing && date.TimezoneText == nil {
		return errors.New("present email timezone requires text")
	}
	if date.UTC != nil && !emailUTCPattern.MatchString(*date.UTC) {
		return errors.New("email UTC timestamp is invalid")
	}
	if date.State == EmailDateParsed {
		if err := validateParsedEmailDate(date); err != nil {
			return err
		}
	}
	leapSecond := false
	for _, diagnostic := range date.Diagnostics {
		if diagnostic.Code == EmailDiagnosticLeapSecondInstantUnavailable {
			leapSecond = true
		}
	}
	if (date.TimezoneState == EmailTimezoneNumeric || date.TimezoneState == EmailTimezoneKnownNamed) && date.State == EmailDateParsed && date.UTC == nil && !leapSecond {
		return errors.New("known email timezone requires UTC")
	}
	if (date.TimezoneState == EmailTimezoneMissing || date.TimezoneState == EmailTimezoneUnknownNamed || date.TimezoneState == EmailTimezoneInvalid) && date.UTC != nil {
		return errors.New("uncertain email timezone cannot have UTC")
	}
	for _, diagnostic := range date.Diagnostics {
		if err := validateEmailDiagnostic(diagnostic, parts, limits); err != nil {
			return err
		}
	}
	return nil
}

func validateParsedEmailDate(date EmailDateV1) error {
	civil := *date.Civil
	atoi := func(start, end int) int {
		value, _ := strconv.Atoi(civil[start:end])
		return value
	}
	year, month, day := atoi(0, 4), atoi(5, 7), atoi(8, 10)
	hour, minute, second := atoi(11, 13), atoi(14, 16), atoi(17, 19)
	if year < 1 || month < 1 || month > 12 || hour > 23 || minute > 59 || second > 60 {
		return errors.New("email civil date components are invalid")
	}
	checkSecond := min(second, 59)
	civilTime := time.Date(year, time.Month(month), day, hour, minute, checkSecond, 0, time.UTC)
	if civilTime.Year() != year || int(civilTime.Month()) != month || civilTime.Day() != day {
		return errors.New("email civil calendar date is invalid")
	}
	if second == 60 {
		if date.UTC != nil || countEmailDateDiagnostic(date, EmailDiagnosticLeapSecondInstantUnavailable) != 1 {
			return errors.New("email leap second implications are invalid")
		}
	} else if countEmailDateDiagnostic(date, EmailDiagnosticLeapSecondInstantUnavailable) != 0 {
		return errors.New("non-leap email date has leap diagnostic")
	}
	var offset int
	switch date.TimezoneState {
	case EmailTimezoneMissing:
		if date.TimezoneText != nil || date.UTC != nil {
			return errors.New("missing email timezone implications are invalid")
		}
		return nil
	case EmailTimezoneNumeric:
		if date.TimezoneText == nil {
			return errors.New("numeric email timezone requires text")
		}
		zone := *date.TimezoneText
		if len(zone) != 5 || zone[0] != '+' && zone[0] != '-' || !emailASCIIDigits(zone[1:]) {
			return errors.New("numeric email timezone is invalid")
		}
		hours, hourErr := strconv.Atoi(zone[1:3])
		minutes, minuteErr := strconv.Atoi(zone[3:])
		if hourErr != nil || minuteErr != nil || hours > 23 || minutes > 59 {
			return errors.New("numeric email timezone is invalid")
		}
		offset = hours*60 + minutes
		if zone[0] == '-' {
			offset = -offset
		}
	case EmailTimezoneKnownNamed:
		if date.TimezoneText == nil {
			return errors.New("named email timezone requires text")
		}
		var ok bool
		offset, ok = emailNamedZoneOffsets[strings.ToUpper(*date.TimezoneText)]
		if !ok || !emailASCIIAlpha(*date.TimezoneText) {
			return errors.New("known email timezone is outside the fixed profile")
		}
	case EmailTimezoneUnknownNamed:
		if date.TimezoneText == nil || !emailASCIIAlpha(*date.TimezoneText) {
			return errors.New("unknown named email timezone is invalid")
		}
		if _, known := emailNamedZoneOffsets[strings.ToUpper(*date.TimezoneText)]; known || date.UTC != nil || countEmailDateDiagnostic(date, EmailDiagnosticTimezoneUnknown) != 1 {
			return errors.New("unknown named email timezone implications are invalid")
		}
		return nil
	default:
		return errors.New("parsed email timezone state is invalid")
	}
	if second == 60 {
		return nil
	}
	if date.UTC == nil {
		return errors.New("known email timezone requires UTC")
	}
	want := civilTime.Add(-time.Duration(offset) * time.Minute).Format(time.RFC3339)
	if *date.UTC != want {
		return errors.New("email UTC timestamp disagrees with civil time and timezone")
	}
	return nil
}

func emailASCIIDigits(value string) bool {
	for _, char := range []byte(value) {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != ""
}

func countEmailDateDiagnostic(date EmailDateV1, code EmailDiagnosticCode) int {
	count := 0
	for _, diagnostic := range date.Diagnostics {
		if diagnostic.Code == code {
			count++
		}
	}
	return count
}

func emailASCIIAlpha(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range []byte(value) {
		if char < 'A' || char > 'Z' && char < 'a' || char > 'z' {
			return false
		}
	}
	return true
}

func validEmailHeaderName(value string) bool {
	for _, char := range []byte(value) {
		if char < 33 || char > 126 || strings.ContainsRune("()<>@,;:\\\"/[]?={} ", rune(char)) {
			return false
		}
	}
	return value != ""
}

func validEmailMediaType(value string) bool {
	if value == "" || value != strings.ToLower(value) || strings.Count(value, "/") != 1 {
		return false
	}
	for component := range strings.SplitSeq(value, "/") {
		if !validEmailHeaderName(component) {
			return false
		}
	}
	return true
}

func emailMessageDisplayBytes(message EmailMessageV1) int64 {
	var total int64
	for _, fields := range [][]EmailDecodedFieldV1{message.Fields.Subject, message.Fields.From, message.Fields.To, message.Fields.Cc, message.Fields.Bcc, message.Fields.ReplyTo, message.Fields.MessageID, message.Fields.InReplyTo, message.Fields.References} {
		for _, field := range fields {
			if field.Text != nil {
				total += int64(len(*field.Text))
			}
			if field.Addresses != nil {
				for _, address := range *field.Addresses {
					total += int64(len(address.Name) + len(address.Address))
				}
			}
		}
	}
	return total
}

func compareEmailPartPaths(left, right string) int {
	a := strings.Split(left, ".")
	b := strings.Split(right, ".")
	for index := range min(len(a), len(b)) {
		av, _ := strconv.Atoi(a[index])
		bv, _ := strconv.Atoi(b[index])
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}

func validateEmailFailure(f EmailFailureV1, limits EmailLimitsV1, top bool) error {
	if top && f.Code != EmailDiagnosticSourceSizeLimit && f.Code != EmailDiagnosticSourceUnsupported && f.Code != EmailDiagnosticInventoryMetadataLimit && f.Code != EmailDiagnosticLimit {
		return errors.New("email top-level failure code is invalid")
	}
	if !validDiagnosticPair(f.Operation, f.Code) || !utf8.ValidString(f.Detail) || len(f.Detail) > limits.DiagnosticDetailBytes {
		return errors.New("email failure is invalid")
	}
	if f.Path != nil {
		if err := ValidateEmailPartPath(*f.Path); err != nil {
			return err
		}
	}
	if (f.Limit != nil && (*f.Limit < 0 || *f.Limit > maxSafeJSONInteger)) || (f.Observed != nil && (*f.Observed < 0 || *f.Observed > maxSafeJSONInteger)) {
		return errors.New("email failure bound is invalid")
	}
	if diagnosticHasLimit(f.Code) {
		if f.Limit == nil || f.Observed == nil || *f.Observed <= *f.Limit {
			return errors.New("email limit failure requires an above-limit observation")
		}
	} else if f.Limit != nil || f.Observed != nil {
		return errors.New("non-limit email failure cannot carry bounds")
	}
	return nil
}

func diagnosticHasLimit(code EmailDiagnosticCode) bool {
	switch code {
	case EmailDiagnosticSourceSizeLimit, EmailDiagnosticHeaderBytesLimit, EmailDiagnosticHeaderTotalBytesLimit, EmailDiagnosticHeaderFieldsLimit, EmailDiagnosticHeaderTotalFieldsLimit, EmailDiagnosticHeaderDisplayLimit, EmailDiagnosticPartBytesLimit, EmailDiagnosticDecodedBytesLimit, EmailDiagnosticPartCountLimit, EmailDiagnosticDepthLimit, EmailDiagnosticBodyUTF8Limit, EmailDiagnosticBodyUTF8TotalLimit, EmailDiagnosticHTMLDisplayLimit, EmailDiagnosticInventoryMetadataLimit, EmailDiagnosticLimit:
		return true
	default:
		return false
	}
}

func validateEmailTermination(t EmailTerminationV1, limits EmailLimitsV1) error {
	return validateEmailFailure(EmailFailureV1{Code: t.Code, Operation: t.Operation, Path: t.Path, Limit: t.Limit, Observed: t.Observed, Detail: ""}, limits, false)
}

func validateEmailDiagnostic(d EmailDiagnosticV1, parts map[string]EmailPartV1, limits EmailLimitsV1) error {
	if !validDiagnosticPair(d.Operation, d.Code) || !utf8.ValidString(d.Detail) || len(d.Detail) > limits.DiagnosticDetailBytes {
		return errors.New("email diagnostic is invalid")
	}
	if d.Path != nil {
		part, ok := parts[*d.Path]
		if !ok {
			return errors.New("email diagnostic path is invalid")
		}
		if d.HeaderIndex != nil && (*d.HeaderIndex < 0 || *d.HeaderIndex >= len(part.Headers)) {
			return errors.New("email diagnostic header index is invalid")
		}
	} else if d.HeaderIndex != nil {
		return errors.New("email diagnostic header index requires path")
	}
	return nil
}

var emailDiagnosticPairs = map[EmailOperation]map[EmailDiagnosticCode]bool{
	EmailOperationSource:        {EmailDiagnosticSourceSizeLimit: true, EmailDiagnosticSourceUnsupported: true},
	EmailOperationHeaders:       {EmailDiagnosticHeaderBytesLimit: true, EmailDiagnosticHeaderTotalBytesLimit: true, EmailDiagnosticHeaderFieldsLimit: true, EmailDiagnosticHeaderTotalFieldsLimit: true, EmailDiagnosticMalformedHeader: true, EmailDiagnosticMissingHeader: true, EmailDiagnosticInvalidHeader: true, EmailDiagnosticDuplicateHeader: true},
	EmailOperationStructure:     {EmailDiagnosticBoundaryMissing: true, EmailDiagnosticBoundaryInvalid: true, EmailDiagnosticBoundaryUnclosed: true, EmailDiagnosticPartCountLimit: true, EmailDiagnosticDepthLimit: true, EmailDiagnosticSignatureUnverified: true, EmailDiagnosticEncryptedUnavailable: true},
	EmailOperationTransfer:      {EmailDiagnosticTransferUnsupported: true, EmailDiagnosticTransferInvalid: true, EmailDiagnosticPartBytesLimit: true, EmailDiagnosticDecodedBytesLimit: true},
	EmailOperationCharset:       {EmailDiagnosticCharsetMissing: true, EmailDiagnosticCharsetUnsupported: true, EmailDiagnosticCharsetInvalid: true, EmailDiagnosticCharsetReplacement: true, EmailDiagnosticBodyUTF8Limit: true, EmailDiagnosticBodyUTF8TotalLimit: true},
	EmailOperationHeaderDisplay: {EmailDiagnosticHeaderDisplayLimit: true, EmailDiagnosticEncodedWordInvalid: true},
	EmailOperationFilename:      {EmailDiagnosticFilenameInvalid: true, EmailDiagnosticFilenameUnsupported: true, EmailDiagnosticHeaderDisplayLimit: true},
	EmailOperationDate:          {EmailDiagnosticDateMissing: true, EmailDiagnosticDateInvalid: true, EmailDiagnosticDateAmbiguous: true, EmailDiagnosticTimezoneUnknown: true, EmailDiagnosticTimezoneOriginUnknown: true, EmailDiagnosticLeapSecondInstantUnavailable: true},
	EmailOperationBodySelection: {EmailDiagnosticBodyUnavailable: true, EmailDiagnosticHTMLDisplayLimit: true},
	EmailOperationCID:           {EmailDiagnosticContentIDInvalid: true, EmailDiagnosticContentIDAmbiguous: true, EmailDiagnosticCIDMissing: true, EmailDiagnosticCIDAmbiguous: true, EmailDiagnosticHeaderDisplayLimit: true},
	EmailOperationInventory:     {EmailDiagnosticInventoryMetadataLimit: true, EmailDiagnosticLimit: true},
}

func validDiagnosticPair(operation EmailOperation, code EmailDiagnosticCode) bool {
	return emailDiagnosticPairs[operation][code]
}
