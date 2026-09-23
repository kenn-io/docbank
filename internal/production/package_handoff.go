package production

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	RecipientFinalTransmittalContractV1 = "production-final-transmittal/v1"
	PackageDeliveryReceiptContractV1    = "production-delivery-receipt/v1"
	maxDeliveryProofBytes               = 16 << 20
)

type RecipientFinalTransmittal struct {
	Contract       string                       `json:"contract"`
	ArchiveSHA256  string                       `json:"archive_sha256"`
	ManifestSHA256 string                       `json:"manifest_sha256"`
	ProfileID      string                       `json:"profile_id"`
	Documents      int                          `json:"documents"`
	Pages          int                          `json:"pages"`
	FirstPage      string                       `json:"first_page"`
	LastPage       string                       `json:"last_page"`
	Volumes        []RecipientTransmittalVolume `json:"volumes"`
}

type RecipientTransmittalVolume struct {
	Name      string `json:"name"`
	Documents int    `json:"documents"`
	Pages     int    `json:"pages"`
	Bytes     int64  `json:"bytes"`
	FirstPage string `json:"first_page"`
	LastPage  string `json:"last_page"`
}

func finalRecipientTransmittal(manifest RecipientManifest, qc PackageQC) RecipientFinalTransmittal {
	result := RecipientFinalTransmittal{Contract: RecipientFinalTransmittalContractV1,
		ArchiveSHA256: qc.ArchiveSHA256, ManifestSHA256: qc.ManifestSHA256,
		ProfileID: manifest.ProfileID, Documents: len(manifest.Documents),
		Pages: len(qc.PageNumbers), FirstPage: qc.PageNumbers[0],
		LastPage: qc.PageNumbers[len(qc.PageNumbers)-1],
		Volumes:  make([]RecipientTransmittalVolume, len(manifest.Volumes))}
	for index, volume := range manifest.Volumes {
		result.Volumes[index] = RecipientTransmittalVolume{Name: volume.Name,
			Documents: volume.Documents, Pages: volume.Pages, Bytes: volume.Bytes}
	}
	for _, doc := range manifest.Documents {
		for index := range result.Volumes {
			volume := &result.Volumes[index]
			if volume.Name != doc.Volume {
				continue
			}
			if volume.FirstPage == "" {
				volume.FirstPage = doc.Control
			}
			volume.LastPage = doc.End
			break
		}
	}
	return result
}

// PublishRecipientTransmittal emits only typed public facts and the verified
// final archive hash. It lives outside the sealed archive to avoid a self hash.
func PublishRecipientTransmittal(archivePath, transmittalPath string,
	manifest RecipientManifest, qc PackageQC) error {
	if archivePath == "" || transmittalPath == "" ||
		VerifyRecipientArchiveWithQC(archivePath, qc) != nil || len(qc.PageNumbers) == 0 {
		return ErrRecipientArchive
	}
	manifestData, err := canonical.Marshal(manifest)
	if err != nil {
		return ErrRecipientArchive
	}
	manifestDigest := sha256.Sum256(manifestData)
	if hex.EncodeToString(manifestDigest[:]) != qc.ManifestSHA256 ||
		len(manifest.Volumes) == 0 || len(manifest.Documents) == 0 {
		return ErrRecipientArchive
	}
	transmittal := finalRecipientTransmittal(manifest, qc)
	data, err := packageJSON(transmittal)
	if err != nil {
		return err
	}
	return publishImmutablePackageSidecar(transmittalPath, data)
}

func ReadRecipientTransmittal(path string) (RecipientFinalTransmittal, error) {
	data, err := readPackageSidecar(path)
	if err != nil {
		return RecipientFinalTransmittal{}, err
	}
	var transmittal RecipientFinalTransmittal
	if err := json.Unmarshal(data, &transmittal, json.RejectUnknownMembers(true)); err != nil ||
		transmittal.Contract != RecipientFinalTransmittalContractV1 ||
		!canonical.IsSHA256Hex(transmittal.ArchiveSHA256) ||
		!canonical.IsSHA256Hex(transmittal.ManifestSHA256) ||
		len(transmittal.Volumes) == 0 || transmittal.Documents < 1 || transmittal.Pages < 1 {
		return RecipientFinalTransmittal{}, ErrRecipientArchive
	}
	canonicalData, err := packageJSON(transmittal)
	if err != nil || !bytes.Equal(canonicalData, data) {
		return RecipientFinalTransmittal{}, ErrRecipientArchive
	}
	return transmittal, nil
}

// PackageDeliveryPolicy is a caller supplied, frozen policy for recording
// evidence of a completed external transfer. This code does not send data.
type PackageDeliveryPolicy struct {
	RecipientCode  string   `json:"recipient_code"`
	AllowedMethods []string `json:"allowed_methods"`
}

type PackageDeliveryEvidence struct {
	RecipientCode string
	Method        string
	DeliveredAt   string
	ProofPath     string
}

type PackageDeliveryReceipt struct {
	Contract             string `json:"contract"`
	ArchiveSHA256        string `json:"archive_sha256"`
	ManifestSHA256       string `json:"manifest_sha256"`
	PackageQCSHA256      string `json:"package_qc_sha256"`
	TransmittalSHA256    string `json:"transmittal_sha256"`
	DeliveryPolicySHA256 string `json:"delivery_policy_sha256"`
	RecipientCode        string `json:"recipient_code"`
	Method               string `json:"method"`
	DeliveredAt          string `json:"delivered_at"`
	ProofSHA256          string `json:"proof_sha256"`
	ProofBytes           int64  `json:"proof_bytes"`
}

func validRecipientCode(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func validDeliveryMethod(value string) bool {
	return value == "secure-transfer" || value == "offline-media"
}

func deliveryPolicyDigest(value PackageDeliveryPolicy) (string, error) {
	if !validRecipientCode(value.RecipientCode) || len(value.AllowedMethods) == 0 || len(value.AllowedMethods) > 2 {
		return "", ErrRecipientArchive
	}
	methods := slices.Clone(value.AllowedMethods)
	slices.Sort(methods)
	for index, method := range methods {
		if !validDeliveryMethod(method) || index > 0 && methods[index-1] == method {
			return "", ErrRecipientArchive
		}
	}
	data, err := canonical.Marshal(PackageDeliveryPolicy{RecipientCode: value.RecipientCode, AllowedMethods: methods})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func packageHandoffInputs(archivePath, qcPath, transmittalPath string) (PackageQC, RecipientFinalTransmittal, string, string, error) {
	qc, err := ReadPackageQCReceipt(qcPath)
	if err != nil || VerifyRecipientArchiveWithQC(archivePath, qc) != nil {
		return PackageQC{}, RecipientFinalTransmittal{}, "", "", ErrRecipientArchive
	}
	transmittal, err := ReadRecipientTransmittal(transmittalPath)
	if err != nil || transmittal.ArchiveSHA256 != qc.ArchiveSHA256 || transmittal.ManifestSHA256 != qc.ManifestSHA256 ||
		transmittal.Pages != len(qc.PageNumbers) || transmittal.FirstPage != qc.PageNumbers[0] ||
		transmittal.LastPage != qc.PageNumbers[len(qc.PageNumbers)-1] {
		return PackageQC{}, RecipientFinalTransmittal{}, "", "", ErrRecipientArchive
	}
	manifest, err := recipientManifestFromVerifiedArchive(archivePath, qc.ManifestSHA256)
	if err != nil {
		return PackageQC{}, RecipientFinalTransmittal{}, "", "", err
	}
	expectedTransmittal, err := packageJSON(finalRecipientTransmittal(manifest, qc))
	if err != nil {
		return PackageQC{}, RecipientFinalTransmittal{}, "", "", err
	}
	actualTransmittal, err := packageJSON(transmittal)
	if err != nil || !bytes.Equal(expectedTransmittal, actualTransmittal) {
		return PackageQC{}, RecipientFinalTransmittal{}, "", "", ErrRecipientArchive
	}
	qcData, err := packageJSON(qc)
	if err != nil {
		return PackageQC{}, RecipientFinalTransmittal{}, "", "", err
	}
	transmittalData, err := packageJSON(transmittal)
	if err != nil {
		return PackageQC{}, RecipientFinalTransmittal{}, "", "", err
	}
	qcDigest, transmittalDigest := sha256.Sum256(qcData), sha256.Sum256(transmittalData)
	return qc, transmittal, hex.EncodeToString(qcDigest[:]), hex.EncodeToString(transmittalDigest[:]), nil
}

func recipientManifestFromVerifiedArchive(path, expectedSHA string) (RecipientManifest, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return RecipientManifest{}, ErrRecipientArchive
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if entry.Name != "MANIFEST.json" {
			continue
		}
		data, err := readPackageEntry(entry, maxPackageMetadataBytes)
		if err != nil {
			return RecipientManifest{}, ErrRecipientArchive
		}
		var manifest RecipientManifest
		if err := json.Unmarshal(data, &manifest, json.RejectUnknownMembers(true)); err != nil {
			return RecipientManifest{}, ErrRecipientArchive
		}
		canonicalData, err := packageJSON(manifest)
		if err != nil || !bytes.Equal(data, canonicalData) {
			return RecipientManifest{}, ErrRecipientArchive
		}
		digest := sha256.Sum256(bytes.TrimSuffix(data, []byte{'\n'}))
		if hex.EncodeToString(digest[:]) != expectedSHA {
			return RecipientManifest{}, ErrRecipientArchive
		}
		return manifest, nil
	}
	return RecipientManifest{}, ErrRecipientArchive
}

func validateDeliveryEvidence(policy PackageDeliveryPolicy, evidence PackageDeliveryEvidence) (string, error) {
	policySHA, err := deliveryPolicyDigest(policy)
	if err != nil || !validRecipientCode(evidence.RecipientCode) || evidence.RecipientCode != policy.RecipientCode ||
		!slices.Contains(policy.AllowedMethods, evidence.Method) || evidence.ProofPath == "" ||
		!strings.HasSuffix(evidence.DeliveredAt, "Z") {
		return "", ErrRecipientArchive
	}
	when, err := time.Parse(time.RFC3339, evidence.DeliveredAt)
	if err != nil || when.UTC().Format(time.RFC3339) != evidence.DeliveredAt {
		return "", ErrRecipientArchive
	}
	return policySHA, nil
}

func digestDeliveryProof(path string, excludedPaths ...string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, ErrRecipientArchive
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxDeliveryProofBytes {
		return "", 0, ErrRecipientArchive
	}
	for _, excluded := range excludedPaths {
		other, statErr := os.Stat(excluded)
		if statErr == nil && os.SameFile(info, other) {
			return "", 0, ErrRecipientArchive
		}
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(file, maxDeliveryProofBytes+1))
	if err != nil || n != info.Size() {
		return "", 0, ErrRecipientArchive
	}
	return hex.EncodeToString(digest.Sum(nil)), n, nil
}

// RecordPackageDelivery only records caller supplied evidence of a completed
// external delivery. It rechecks the actual archive, QC and public transmittal
// at handoff and writes an immutable receipt outside the archive.
func RecordPackageDelivery(archivePath, qcPath, transmittalPath, receiptPath string,
	policy PackageDeliveryPolicy, evidence PackageDeliveryEvidence) (PackageDeliveryReceipt, error) {
	qc, _, qcSHA, transmittalSHA, err := packageHandoffInputs(archivePath, qcPath, transmittalPath)
	if err != nil {
		return PackageDeliveryReceipt{}, err
	}
	policySHA, err := validateDeliveryEvidence(policy, evidence)
	if err != nil {
		return PackageDeliveryReceipt{}, err
	}
	proofSHA, proofBytes, err := digestDeliveryProof(evidence.ProofPath,
		archivePath, qcPath, transmittalPath, receiptPath)
	if err != nil {
		return PackageDeliveryReceipt{}, err
	}
	receipt := PackageDeliveryReceipt{Contract: PackageDeliveryReceiptContractV1,
		ArchiveSHA256: qc.ArchiveSHA256, ManifestSHA256: qc.ManifestSHA256,
		PackageQCSHA256: qcSHA, TransmittalSHA256: transmittalSHA,
		DeliveryPolicySHA256: policySHA, RecipientCode: evidence.RecipientCode,
		Method: evidence.Method, DeliveredAt: evidence.DeliveredAt,
		ProofSHA256: proofSHA, ProofBytes: proofBytes}
	data, err := packageJSON(receipt)
	if err != nil {
		return PackageDeliveryReceipt{}, err
	}
	if err := publishImmutablePackageSidecar(receiptPath, data); err != nil {
		return PackageDeliveryReceipt{}, err
	}
	return receipt, nil
}

func VerifyPackageDeliveryReceipt(archivePath, qcPath, transmittalPath, receiptPath, proofPath string,
	policy PackageDeliveryPolicy) error {
	qc, _, qcSHA, transmittalSHA, err := packageHandoffInputs(archivePath, qcPath, transmittalPath)
	if err != nil {
		return err
	}
	policySHA, err := deliveryPolicyDigest(policy)
	if err != nil {
		return err
	}
	data, err := readPackageSidecar(receiptPath)
	if err != nil {
		return err
	}
	var receipt PackageDeliveryReceipt
	if err := json.Unmarshal(data, &receipt, json.RejectUnknownMembers(true)); err != nil {
		return ErrRecipientArchive
	}
	canonicalData, err := packageJSON(receipt)
	if err != nil || !bytes.Equal(data, canonicalData) || receipt.Contract != PackageDeliveryReceiptContractV1 ||
		receipt.ArchiveSHA256 != qc.ArchiveSHA256 || receipt.ManifestSHA256 != qc.ManifestSHA256 ||
		receipt.PackageQCSHA256 != qcSHA || receipt.TransmittalSHA256 != transmittalSHA ||
		receipt.DeliveryPolicySHA256 != policySHA || receipt.RecipientCode != policy.RecipientCode ||
		!slices.Contains(policy.AllowedMethods, receipt.Method) || receipt.ProofBytes < 1 ||
		!canonical.IsSHA256Hex(receipt.ProofSHA256) {
		return ErrRecipientArchive
	}
	_, err = validateDeliveryEvidence(policy, PackageDeliveryEvidence{RecipientCode: receipt.RecipientCode,
		Method: receipt.Method, DeliveredAt: receipt.DeliveredAt, ProofPath: "recorded-outside-archive"})
	if err != nil {
		return err
	}
	proofSHA, proofBytes, err := digestDeliveryProof(proofPath, archivePath, qcPath, transmittalPath, receiptPath)
	if err != nil || proofSHA != receipt.ProofSHA256 || proofBytes != receipt.ProofBytes {
		return ErrRecipientArchive
	}
	return nil
}

func readPackageSidecar(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPackageMetadataBytes+1))
	if err != nil || len(data) > maxPackageMetadataBytes {
		return nil, ErrRecipientArchive
	}
	return data, nil
}

func publishImmutablePackageSidecar(path string, data []byte) error {
	if path == "" || len(data) == 0 || len(data) > maxPackageMetadataBytes {
		return ErrRecipientArchive
	}
	current, err := readPackageSidecar(path)
	if err == nil {
		if bytes.Equal(current, data) {
			return nil
		}
		return ErrRecipientArchive
	}
	if !errors.Is(err, os.ErrNotExist) {
		return ErrRecipientArchive
	}
	staged, err := os.CreateTemp(filepath.Dir(path), ".production-sidecar-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	if _, err := staged.Write(data); err != nil {
		return err
	}
	if err := staged.Sync(); err != nil {
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	if err := os.Chmod(staged.Name(), 0o444); err != nil {
		return err
	}
	if err := os.Link(staged.Name(), path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		current, readErr := readPackageSidecar(path)
		if readErr == nil && bytes.Equal(current, data) {
			return nil
		}
		return ErrRecipientArchive
	}
	return nil
}
