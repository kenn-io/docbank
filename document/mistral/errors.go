package mistral

import "errors"

// IsRetryable reports whether err is a transient provider or transport failure,
// or a spool-capacity refusal that an application can schedule again later.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrTransientResponse) || errors.Is(err, ErrSpoolCapacity)
}
