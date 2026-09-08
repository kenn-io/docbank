package cohereapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyStatusProvidesBoundedNeutralRetryGuidance(t *testing.T) {
	for _, test := range []struct {
		status int
		kind   StatusKind
	}{
		{status: 408, kind: StatusTransient},
		{status: 429, kind: StatusTransient},
		{status: 500, kind: StatusTransient},
		{status: 413, kind: StatusCapacity},
		{status: 400, kind: StatusPermanent},
		{status: 307, kind: StatusPermanent},
	} {
		result := ClassifyStatus(test.status, "", time.Time{})
		assert.Equal(t, test.kind, result.Kind)
	}

	for _, seconds := range []int64{math.MaxInt64/int64(time.Second) + 1, math.MaxInt64} {
		result := ClassifyStatus(429, strconv.FormatInt(seconds, 10), time.Time{})
		assert.Equal(t, StatusTransient, result.Kind)
		assert.True(t, result.RetrySet)
		assert.Equal(t, time.Hour, result.RetryDelay)
	}
}

func TestValidTokenRejectsUnsafeOrOversizedValues(t *testing.T) {
	assert.True(t, ValidToken("safe-token", 10))
	for _, value := range []string{"", "has space", "line\nbreak", string([]byte{0xff}), "too-long-token"} {
		assert.False(t, ValidToken(value, 10))
	}
}

func TestIsJSONContentTypeAcceptsOnlyExactJSONWithOptionalUTF8(t *testing.T) {
	assert.True(t, IsJSONContentType("application/json"))
	assert.True(t, IsJSONContentType("application/json; charset=UTF-8"))
	for _, value := range []string{"", "text/json", "application/json; charset=latin1", "application/json; version=2"} {
		assert.False(t, IsJSONContentType(value))
	}
}

func TestReadBoundedDistinguishesCapacityCancellationAndIOFailure(t *testing.T) {
	body, outcome, err := ReadBounded(context.Background(), bytes.NewBufferString("okay"), 4)
	require.NoError(t, err)
	assert.Equal(t, ReadOK, outcome)
	assert.Equal(t, []byte("okay"), body)

	body, outcome, err = ReadBounded(context.Background(), bytes.NewBufferString("large"), 4)
	require.NoError(t, err)
	assert.Nil(t, body)
	assert.Equal(t, ReadCapacity, outcome)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body, outcome, err = ReadBounded(ctx, bytes.NewBufferString("okay"), 4)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, body)
	assert.Equal(t, ReadCanceled, outcome)

	body, outcome, err = ReadBounded(context.Background(), errorReader{}, 4)
	require.ErrorIs(t, err, ErrResponseRead)
	assert.Nil(t, body)
	assert.Equal(t, ReadTransient, outcome)
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("private read failure") }

var _ io.Reader = errorReader{}
