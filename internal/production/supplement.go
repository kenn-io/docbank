package production

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

const SupplementRecordContractV1 = "production-supplement/v1"

var ErrSupplementConflict = errors.New("supplement conflicts with production authority")

// SupplementRequest pins the published parent and the newly prepared child.
// The operation ID is distinct from the child job's Bates allocation identity.
type SupplementRequest struct {
	OperationID         string `json:"operation_id"`
	ParentJobID         string `json:"parent_job_id"`
	JobID               string `json:"job_id"`
	ParentReceiptSHA256 string `json:"parent_receipt_sha256"`
	PreparedSHA256      string `json:"prepared_sha256"`
	PreparedInputSHA256 string `json:"prepared_input_sha256"`
}

// SupplementRecord is immutable linkage between one published parent, one
// separately prepared child and the existing Bates ledger's continuation.
type SupplementRecord struct {
	Contract                string `json:"contract"`
	OperationID             string `json:"operation_id"`
	ParentJobID             string `json:"parent_job_id"`
	JobID                   string `json:"job_id"`
	ParentReceiptSHA256     string `json:"parent_receipt_sha256"`
	PreparedSHA256          string `json:"prepared_sha256"`
	PreparedInputSHA256     string `json:"prepared_input_sha256"`
	RequestSHA256           string `json:"request_sha256"`
	SetID                   string `json:"set_id"`
	Revision                int64  `json:"revision"`
	NamespaceID             string `json:"namespace_id"`
	ParentAllocationID      string `json:"parent_allocation_id"`
	AllocationID            string `json:"allocation_id"`
	NumberReservationSHA256 string `json:"number_reservation_sha256"`
	ParentEndSequence       int64  `json:"parent_end_sequence"`
	StartSequence           int64  `json:"start_sequence"`
	EndSequence             int64  `json:"end_sequence"`
	CreatedAt               string `json:"created_at"`
	SHA256                  string `json:"sha256,omitzero"`
}

func SupplementRequestSHA256(request SupplementRequest) (string, error) {
	if !canonicalUUIDv4(request.OperationID) || !canonicalUUIDv4(request.ParentJobID) ||
		!canonicalUUIDv4(request.JobID) || request.ParentJobID == request.JobID ||
		!canonicalSHA256(request.ParentReceiptSHA256) || !canonicalSHA256(request.PreparedSHA256) ||
		!canonicalSHA256(request.PreparedInputSHA256) {
		return "", ErrSupplementConflict
	}
	raw, err := canonical.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func CanonicalSupplementRecord(record SupplementRecord) ([]byte, string, error) {
	if !validSupplementRecordFields(record) {
		return nil, "", ErrSupplementConflict
	}
	record.SHA256 = ""
	raw, err := canonical.Marshal(record)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}

func ValidateSupplementRecord(record SupplementRecord) error {
	_, digest, err := CanonicalSupplementRecord(record)
	if err != nil || digest != record.SHA256 {
		return ErrSupplementConflict
	}
	return nil
}

func validSupplementRecordFields(record SupplementRecord) bool {
	if record.Contract != SupplementRecordContractV1 || !canonicalUUIDv4(record.OperationID) ||
		!canonicalUUIDv4(record.ParentJobID) || !canonicalUUIDv4(record.JobID) ||
		record.ParentJobID == record.JobID || !canonicalUUIDv4(record.SetID) || record.Revision < 1 ||
		!canonicalUUIDv4(record.NamespaceID) || !canonicalUUIDv4(record.ParentAllocationID) ||
		!canonicalUUIDv4(record.AllocationID) || record.ParentAllocationID == record.AllocationID ||
		!canonicalSHA256(record.ParentReceiptSHA256) || !canonicalSHA256(record.PreparedSHA256) ||
		!canonicalSHA256(record.PreparedInputSHA256) || !canonicalSHA256(record.RequestSHA256) ||
		!canonicalSHA256(record.NumberReservationSHA256) || record.ParentEndSequence < 1 ||
		record.StartSequence <= record.ParentEndSequence || record.EndSequence < record.StartSequence {
		return false
	}
	created, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
	return err == nil && created.Location() == time.UTC && created.Format(time.RFC3339Nano) == record.CreatedAt
}
