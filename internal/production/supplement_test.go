package production

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSupplementRequestAndRecordBindExactParentChildAndRange(t *testing.T) {
	request := SupplementRequest{
		OperationID:         "11111111-1111-4111-8111-111111111111",
		ParentJobID:         "22222222-2222-4222-8222-222222222222",
		JobID:               "33333333-3333-4333-8333-333333333333",
		ParentReceiptSHA256: strings.Repeat("a", 64),
		PreparedSHA256:      strings.Repeat("b", 64),
		PreparedInputSHA256: strings.Repeat("c", 64),
	}
	requestSHA, err := SupplementRequestSHA256(request)
	require.NoError(t, err)
	changedRequest := request
	changedRequest.PreparedInputSHA256 = strings.Repeat("d", 64)
	changedSHA, err := SupplementRequestSHA256(changedRequest)
	require.NoError(t, err)
	require.NotEqual(t, requestSHA, changedSHA)
	changedRequest.ParentJobID = changedRequest.JobID
	_, err = SupplementRequestSHA256(changedRequest)
	require.ErrorIs(t, err, ErrSupplementConflict)

	record := SupplementRecord{
		Contract: SupplementRecordContractV1, OperationID: request.OperationID,
		ParentJobID: request.ParentJobID, JobID: request.JobID,
		ParentReceiptSHA256: request.ParentReceiptSHA256,
		PreparedSHA256:      request.PreparedSHA256, PreparedInputSHA256: request.PreparedInputSHA256,
		RequestSHA256: requestSHA, SetID: "44444444-4444-4444-8444-444444444444", Revision: 2,
		NamespaceID:             "55555555-5555-4555-8555-555555555555",
		ParentAllocationID:      "66666666-6666-4666-8666-666666666666",
		AllocationID:            "77777777-7777-4777-8777-777777777777",
		NumberReservationSHA256: strings.Repeat("e", 64),
		ParentEndSequence:       3, StartSequence: 4, EndSequence: 5,
		CreatedAt: "2026-09-27T12:34:56Z",
	}
	_, record.SHA256, err = CanonicalSupplementRecord(record)
	require.NoError(t, err)
	require.NoError(t, ValidateSupplementRecord(record))
	changedRecord := record
	changedRecord.AllocationID = "88888888-8888-4888-8888-888888888888"
	require.ErrorIs(t, ValidateSupplementRecord(changedRecord), ErrSupplementConflict)
	changedRecord = record
	changedRecord.StartSequence = record.ParentEndSequence
	_, _, err = CanonicalSupplementRecord(changedRecord)
	require.ErrorIs(t, err, ErrSupplementConflict)
	changedRecord = record
	changedRecord.CreatedAt = "2026-09-27T12:34:56.000000000Z"
	_, _, err = CanonicalSupplementRecord(changedRecord)
	require.ErrorIs(t, err, ErrSupplementConflict)
}
