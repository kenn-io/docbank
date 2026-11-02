package packstore

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestClassifyPhysicalErrorPreservesControlErrors(t *testing.T) {
	limit := newLimitError(LimitBlobRawBytes, 2, 1)
	tests := []struct {
		name        string
		input       error
		unavailable bool
		missing     bool
		corrupt     bool
	}{
		{name: "filesystem failure", input: errors.New("filesystem offline"), unavailable: true},
		{name: "missing", input: markPhysicalSourceNotFound(fs.ErrNotExist), missing: true},
		{name: "corrupt", input: pack.ErrChecksum, corrupt: true},
		{name: "already unavailable", input: ErrStoreUnavailable, unavailable: true},
		{name: "context canceled", input: context.Canceled},
		{name: "context deadline", input: context.DeadlineExceeded},
		{name: "verified eof", input: io.EOF},
		{name: "verification incomplete", input: pack.ErrVerificationIncomplete},
		{name: "streams active", input: pack.ErrStreamsActive},
		{name: "invalid policy", input: ErrInvalidPolicy},
		{name: "structured limit", input: limit},
		{name: "stream unsupported", input: pack.ErrStreamUnsupported},
		{name: "stream limit", input: pack.ErrStreamLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			err := classifyPhysicalError(tt.input)

			require.ErrorIs(t, err, tt.input)
			assert.Equal(tt.unavailable, errors.Is(err, ErrStoreUnavailable))
			assert.Equal(tt.missing, errors.Is(err, ErrPhysicalMissing))
			assert.Equal(tt.corrupt, errors.Is(err, ErrPhysicalCorrupt))
		})
	}
}

func TestClassifyIntegrityError(t *testing.T) {
	tests := []struct {
		name    string
		input   error
		corrupt bool
	}{
		{name: "content mismatch", input: ErrContentMismatch, corrupt: true},
		{name: "bad magic", input: pack.ErrBadMagic, corrupt: true},
		{name: "unsupported version", input: pack.ErrUnsupportedVersion, corrupt: true},
		{name: "truncated", input: pack.ErrTruncated, corrupt: true},
		{name: "checksum", input: pack.ErrChecksum, corrupt: true},
		{name: "corrupt", input: pack.ErrCorrupt, corrupt: true},
		{name: "blob mismatch", input: pack.ErrBlobMismatch, corrupt: true},
		{name: "verification incomplete", input: pack.ErrVerificationIncomplete},
		{name: "context canceled", input: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ClassifyIntegrityError(tt.input)

			assert.Equal(t, tt.corrupt, errors.Is(err, ErrPhysicalCorrupt))
			require.ErrorIs(t, err, tt.input)
		})
	}
}

func TestClassifyRepresentationLimitError(t *testing.T) {
	tests := []struct {
		name    string
		input   error
		corrupt bool
	}{
		{
			name: "pack container limit",
			input: newLimitError(
				LimitPackContainerBytes, 2, 1,
			),
			corrupt: true,
		},
		{
			name:    "pack footer limit",
			input:   newLimitError(LimitPackFooterBytes, 2, 1),
			corrupt: true,
		},
		{
			name:    "pack entry limit",
			input:   newLimitError(LimitPackEntryCount, 2, 1),
			corrupt: true,
		},
		{name: "blob raw limit", input: newLimitError(LimitBlobRawBytes, 2, 1)},
		{
			name:    "blob stored limit",
			input:   newLimitError(LimitBlobStoredBytes, 2, 1),
			corrupt: true,
		},
		{name: "blob window limit", input: newLimitError(LimitBlobWindowBytes, 2, 1)},
		{name: "blob stat limit", input: newLimitError(LimitBlobStatBytes, 2, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ClassifyRepresentationLimitError(tt.input)

			assert.Equal(t, tt.corrupt, errors.Is(err, ErrPhysicalCorrupt))
			require.ErrorIs(t, err, tt.input)
		})
	}
}
