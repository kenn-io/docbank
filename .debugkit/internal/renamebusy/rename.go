// Package renamebusy retries renames blocked by Windows file handles.
package renamebusy

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/kit/atomicfile"
)

const (
	// DefaultTimeout bounds retries for one rename.
	DefaultTimeout = 2 * time.Second
	defaultPoll    = 5 * time.Millisecond
)

// Replace retries Windows sharing violations for timeout. It returns the last
// filesystem error. timeout must be positive; a nonpositive elapsed limit
// permits unlimited retries.
func Replace(oldpath, newpath string, timeout time.Duration) error {
	_, err := backoff.Retry(context.Background(), func() (struct{}, error) {
		err := atomicfile.Replace(oldpath, newpath)
		if err != nil && !fileBusy(err) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(backoff.NewConstantBackOff(defaultPoll)),
		backoff.WithMaxElapsedTime(timeout))
	if err != nil {
		return backoff.AsRetryError(err).LastErr
	}
	return nil
}
