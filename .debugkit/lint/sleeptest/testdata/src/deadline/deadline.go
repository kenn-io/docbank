package deadline

import (
	"context"
	"time"
)

// Non-test files are out of scope.
func shortTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Millisecond)
}
