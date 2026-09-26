package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const PassageRefVersionV1 = 1

// PassageRefV1 binds a citation to exact retained source and rendition
// authority. ByteStart and ByteEnd address the UTF-8 Markdown body after its
// machine envelope has been removed.
type PassageRefV1 struct {
	Version             int    `json:"version"`
	FederationDomainUID string `json:"federation_domain_uid,omitzero"`
	VaultUID            string `json:"vault_uid"`
	DocumentUID         string `json:"document_uid"`
	ContentVersionID    string `json:"content_version_id"`
	SourceSHA256        string `json:"source_sha256"`
	RenditionBuildID    string `json:"rendition_build_id"`
	AttachmentID        string `json:"attachment_id"`
	BodySHA256          string `json:"body_sha256"`
	ByteStart           int    `json:"byte_start"`
	ByteEnd             int    `json:"byte_end"`
	QuoteSHA256         string `json:"quote_sha256"`
}

// SlicePassage returns an owned copy of one half-open UTF-8 byte range.
func SlicePassage(body []byte, start, end int) ([]byte, error) {
	if !utf8.Valid(body) || start < 0 || end <= start || end > len(body) {
		return nil, errors.New("invalid passage range")
	}
	if start > 0 && !utf8.RuneStart(body[start]) ||
		end < len(body) && !utf8.RuneStart(body[end]) {
		return nil, errors.New("passage range splits UTF-8")
	}
	return bytes.Clone(body[start:end]), nil
}

// NewPassageRefV1 seals an exact body range into an immutable reference. It
// hashes the bytes as supplied and performs no whitespace or Unicode
// normalization.
func NewPassageRefV1(identity PassageRefV1, body []byte, start, end int) (PassageRefV1, error) {
	quote, err := SlicePassage(body, start, end)
	if err != nil {
		return PassageRefV1{}, err
	}
	identity.Version = PassageRefVersionV1
	identity.ByteStart = start
	identity.ByteEnd = end
	identity.BodySHA256 = passageSHA256(body)
	identity.QuoteSHA256 = passageSHA256(quote)
	if err := ValidatePassageRefV1(identity, body); err != nil {
		return PassageRefV1{}, err
	}
	return identity, nil
}

// ValidatePassageRefV1 verifies both the immutable identity tuple and the
// exact retained body bytes.
func ValidatePassageRefV1(ref PassageRefV1, body []byte) error {
	if err := ValidatePassageIdentityV1(ref); err != nil {
		return err
	}
	if got := passageSHA256(body); got != ref.BodySHA256 {
		return fmt.Errorf("passage body SHA-256 %s differs from reference %s", got, ref.BodySHA256)
	}
	quote, err := SlicePassage(body, ref.ByteStart, ref.ByteEnd)
	if err != nil {
		return err
	}
	if got := passageSHA256(quote); got != ref.QuoteSHA256 {
		return fmt.Errorf("passage quote SHA-256 %s differs from reference %s", got, ref.QuoteSHA256)
	}
	return nil
}

// ValidatePassageIdentityV1 validates fields that can be checked before any
// content is opened. Callers should run authorization before body/hash checks.
func ValidatePassageIdentityV1(ref PassageRefV1) error {
	if err := ValidatePassageAddressV1(ref); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"source SHA-256": ref.SourceSHA256, "rendition build ID": ref.RenditionBuildID,
		"attachment ID": ref.AttachmentID, "body SHA-256": ref.BodySHA256,
		"quote SHA-256": ref.QuoteSHA256,
	} {
		if !passageDigest(value) {
			return fmt.Errorf("passage %s is invalid", name)
		}
	}
	if ref.ByteStart < 0 || ref.ByteEnd <= ref.ByteStart {
		return errors.New("passage byte range is invalid")
	}
	return nil
}

// ValidatePassageAddressV1 validates only the non-content address. Resolvers
// use it before authorization, then validate hashes and ranges only after the
// address has been admitted.
func ValidatePassageAddressV1(ref PassageRefV1) error {
	if ref.Version != PassageRefVersionV1 {
		return errors.New("passage reference version is unsupported")
	}
	for name, value := range map[string]string{
		"vault UID": ref.VaultUID, "document UID": ref.DocumentUID,
		"content version ID": ref.ContentVersionID,
	} {
		if !passageUUID(value) {
			return fmt.Errorf("passage %s is invalid", name)
		}
	}
	if ref.FederationDomainUID != "" && !passageUUID(ref.FederationDomainUID) {
		return errors.New("passage federation domain UID is invalid")
	}
	return nil
}

// PassageIdentityV1 returns the SHA-256 identity of the versioned canonical
// tuple. The canonical form is intentionally independent of JSON encoders.
func PassageIdentityV1(ref PassageRefV1) (string, error) {
	if err := ValidatePassageIdentityV1(ref); err != nil {
		return "", err
	}
	canonical := []string{
		"docbank-passage-ref/v1", ref.FederationDomainUID, ref.VaultUID,
		ref.DocumentUID, ref.ContentVersionID, ref.SourceSHA256,
		ref.RenditionBuildID, ref.AttachmentID, ref.BodySHA256,
		strconv.Itoa(ref.ByteStart), strconv.Itoa(ref.ByteEnd), ref.QuoteSHA256,
	}
	return passageSHA256([]byte(strings.Join(canonical, "\x00"))), nil
}

// RuneRangeToPassageBytes converts existing half-open rune coordinates to
// passage byte coordinates without changing the underlying text.
func RuneRangeToPassageBytes(body []byte, start, end int) (int, int, error) {
	if !utf8.Valid(body) || start < 0 || end <= start {
		return 0, 0, errors.New("invalid passage rune range")
	}
	runeIndex, byteStart, byteEnd := 0, -1, -1
	for byteIndex := range string(body) {
		if runeIndex == start {
			byteStart = byteIndex
		}
		if runeIndex == end {
			byteEnd = byteIndex
			break
		}
		runeIndex++
	}
	if byteStart < 0 && start == runeIndex {
		byteStart = len(body)
	}
	if byteEnd < 0 && end == utf8.RuneCount(body) {
		byteEnd = len(body)
	}
	if byteStart < 0 || byteEnd < 0 || byteStart >= byteEnd {
		return 0, 0, errors.New("passage rune range exceeds body")
	}
	return byteStart, byteEnd, nil
}

func passageSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func passageDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func passageUUID(value string) bool {
	if len(value) != 36 || value != strings.ToLower(value) ||
		value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	return err == nil && len(decoded) == 16 && decoded[6]>>4 == 4 && decoded[8]>>6 == 2
}
