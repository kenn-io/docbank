package pdfstamp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain lets the test binary serve as the supervised worker, as the
// Docbank binary does through its internal-pdfstamp-worker command.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "internal-pdfstamp-worker" {
		if err := RunWorker(context.Background(), os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSupervisedWorkerStampsSelectedPagesEndToEnd(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	ConfigureWorker(executable)
	t.Cleanup(func() { ConfigureWorker("") })
	labels := []PageLabel{{SourcePage: 3, Label: "OUR000041"}, {SourcePage: 6, Label: "OUR000042"}}
	var output bytes.Buffer

	result, err := StampSelectedSupervised(t.Context(), bytes.NewReader(syntheticNumberedPDF(t)), labels, validRecipe(t), &output)

	require.NoError(t, err)
	assert.Equal(t, 2, result.PageCount)
	assert.Equal(t, int64(output.Len()), result.Size)
	verified, err := VerifyStamped(t.Context(), output.Bytes(), labels)
	require.NoError(t, err)
	assert.Equal(t, result.SHA256, verified.SHA256)
}

func TestRunWorkerRejectsMalformedFraming(t *testing.T) {
	frame := func(requestSize uint64, request []byte) []byte {
		var input bytes.Buffer
		require.NoError(t, binary.Write(&input, binary.BigEndian, requestSize))
		input.Write(request)
		return input.Bytes()
	}
	unknown, err := json.Marshal(workerRequest{Operation: "rasterize", Sources: 1})
	require.NoError(t, err)
	unknownInput := frame(uint64(len(unknown)), unknown)
	unknownInput = binary.BigEndian.AppendUint64(unknownInput, 1)
	unknownInput = append(unknownInput, 'x')
	for name, test := range map[string]struct {
		input []byte
		want  string
	}{
		"zero request size":      {frame(0, nil), "request size 0"},
		"oversized request size": {frame(maxWorkerRequestBytes+1, nil), fmt.Sprintf("request size %d", maxWorkerRequestBytes+1)},
		"unknown operation":      {unknownInput, `unknown operation "rasterize"`},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			err := RunWorker(t.Context(), bytes.NewReader(test.input), &output)
			require.ErrorIs(t, err, ErrStampEngineFailure)
			require.ErrorContains(t, err, test.want)
			assert.NotContains(t, err.Error(), "%!")
			assert.Zero(t, output.Len())
		})
	}
}

func TestSupervisedWorkerRejectsOversizedRequestWithSizes(t *testing.T) {
	ConfigureWorker("unused-worker")
	t.Cleanup(func() { ConfigureWorker("") })
	labels := make([]PageLabel, maxWorkerRequestBytes/16)
	for index := range labels {
		labels[index] = PageLabel{SourcePage: index + 1, Label: "OUR000001"}
	}

	_, err := CombineStampedSupervised(t.Context(), [][]byte{[]byte("synthetic")}, labels, &bytes.Buffer{})

	require.ErrorIs(t, err, ErrStampEngineFailure)
	require.ErrorContains(t, err, fmt.Sprintf("the limit is %d", maxWorkerRequestBytes))
	assert.NotContains(t, err.Error(), "%!")
}
