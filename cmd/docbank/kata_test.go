package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/kataevidence"
)

func TestKataCLIRequiresRange(t *testing.T) {
	_, err := runCLI(t, "kata", "cite", "id:2", "--title", "Follow up", "--idempotency-key", "test")
	require.ErrorContains(t, err, "range requires --end")
}

func TestKataContextPrintsEveryStatus(t *testing.T) {
	var out bytes.Buffer
	err := writeKataContext(&out, kataevidence.Context{Passages: []kataevidence.Resolution{
		{State: "unsupported"},
		{State: "failed", SavedQuote: "Saved words"},
	}})
	require.NoError(t, err)
	require.Contains(t, out.String(), "unsupported\nfailed\nSaved words\n")
}

type kataFailWriter struct {
	writes int
	failAt int
}

func (w *kataFailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func TestKataContextPropagatesOutputErrors(t *testing.T) {
	result := kataevidence.Context{Passages: []kataevidence.Resolution{
		{State: "available", Evidence: kataevidence.Evidence{Reference: kataevidence.Reference{Docbank: &kataevidence.DocumentReference{NodeID: 2}}}, SavedQuote: "hello"},
		{State: "unsupported"},
	}, NextOffset: 10}
	for failAt := 1; failAt <= 5; failAt++ {
		err := writeKataContext(&kataFailWriter{failAt: failAt}, result)
		require.ErrorIs(t, err, io.ErrClosedPipe, "write %d: %v", failAt, err)
	}
}
