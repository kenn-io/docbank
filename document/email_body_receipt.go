package document

import (
	"bytes"
	"errors"
	"strconv"

	"go.kenn.io/docbank/internal/canonical"
)

const EmailBodyReceiptContractV1 = "docbank-email-body-receipt/v1"

// EmailBodyReceiptV1 binds a local search rendition to the exact chosen MIME
// body while retaining the original EML as its source identity.
type EmailBodyReceiptV1 struct {
	ContractVersion       string `json:"contract_version"`
	SourceSHA256          string `json:"source_sha256"`
	SourceSize            int64  `json:"source_size"`
	EmailGenerationID     string `json:"email_generation_id"`
	EmailChecksum         string `json:"email_checksum"`
	PartPath              string `json:"part_path"`
	BodySHA256            string `json:"body_sha256"`
	BodySize              int64  `json:"body_size"`
	BodyRecipeFingerprint string `json:"body_recipe_fingerprint"`
	EvidenceChecksum      string `json:"evidence_checksum"`
	RenditionChecksum     string `json:"rendition_checksum"`
	MarkdownChecksum      string `json:"markdown_checksum"`
}

func validateEmailBodyReceipt(v EmailBodyReceiptV1) error {
	if v.ContractVersion != EmailBodyReceiptContractV1 {
		return errors.New("invalid email body receipt contract")
	}
	if v.SourceSize < 1 || v.SourceSize > 128<<20 || v.BodySize < 1 || v.BodySize > 16<<20 {
		return errors.New("email body receipt size outside available body bounds")
	}
	for _, digest := range []string{v.SourceSHA256, v.EmailGenerationID, v.EmailChecksum, v.BodySHA256, v.BodyRecipeFingerprint, v.EvidenceChecksum, v.RenditionChecksum, v.MarkdownChecksum} {
		if !canonical.IsSHA256Hex(digest) {
			return errors.New("invalid email body receipt digest")
		}
	}
	return ValidateEmailPartPath(v.PartPath)
}
func MarshalEmailBodyReceiptV1(value EmailBodyReceiptV1) ([]byte, string, error) {
	if err := validateEmailBodyReceipt(value); err != nil {
		return nil, "", err
	}
	encoded, err := canonical.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	return encoded, emailSHA256(encoded), nil
}
func DecodeEmailBodyReceiptV1(encoded []byte) (EmailBodyReceiptV1, string, error) {
	if len(encoded) > 4096 {
		return EmailBodyReceiptV1{}, "", errors.New("email body receipt exceeds size limit")
	}
	v, err := canonical.Decode[EmailBodyReceiptV1](encoded)
	if err != nil {
		return v, "", err
	}
	again, checksum, err := MarshalEmailBodyReceiptV1(v)
	if err != nil {
		return v, "", err
	}
	if !bytes.Equal(again, encoded) {
		return v, "", errors.New("email body receipt bytes are not canonical")
	}
	return v, checksum, nil
}
func EmailBodyBuildID(v EmailBodyReceiptV1) (string, error) {
	if err := validateEmailBodyReceipt(v); err != nil {
		return "", err
	}
	return emailTupleHash("docbank-email-body-build/v1", v.SourceSHA256, v.EmailGenerationID, v.PartPath, v.BodyRecipeFingerprint, v.EvidenceChecksum, v.RenditionChecksum), nil
}
func EmailBodyOperationID(bodyRecipeFingerprint string) (string, error) {
	if !canonical.IsSHA256Hex(bodyRecipeFingerprint) {
		return "", errors.New("invalid email body recipe fingerprint")
	}
	return "docbank-email-body:" + bodyRecipeFingerprint, nil
}
func EmailBodyAuthorizationChecksum(sourceSHA256 string, sourceSize int64, bodyRecipeFingerprint string) (string, error) {
	if !canonical.IsSHA256Hex(sourceSHA256) || !canonical.IsSHA256Hex(bodyRecipeFingerprint) || sourceSize < 1 || sourceSize > 128<<20 {
		return "", errors.New("invalid email body local authority source or recipe")
	}
	return emailTupleHash("docbank-email-body-local-authority/v1", sourceSHA256, strconv.FormatInt(sourceSize, 10), bodyRecipeFingerprint), nil
}
