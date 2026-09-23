package document

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type cancelAfterEvidenceTextChecksContext struct {
	checks int
}

func (ctx *cancelAfterEvidenceTextChecksContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (ctx *cancelAfterEvidenceTextChecksContext) Done() <-chan struct{} { return nil }
func (ctx *cancelAfterEvidenceTextChecksContext) Value(any) any         { return nil }

func (ctx *cancelAfterEvidenceTextChecksContext) Err() error {
	ctx.checks++
	if ctx.checks > 2 {
		return context.Canceled
	}
	return nil
}

func TestValidateEvidenceTextContextCancelsMultibyteScan(t *testing.T) {
	ctx := &cancelAfterEvidenceTextChecksContext{}
	value := "x" + strings.Repeat("é", 4_096)

	require.ErrorIs(t, validateEvidenceTextContext(ctx, value, "unit text"), context.Canceled)
}
