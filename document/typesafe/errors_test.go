package typesafe

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestStatusErrorClassifiesDocumentedStatuses(t *testing.T) {
	for _, test := range []struct {
		status int
		kind   error
		retry  time.Duration
	}{
		{http.StatusUnauthorized, ErrPermanentResponse, 0},
		{http.StatusUnprocessableEntity, ErrPermanentResponse, 0},
		{http.StatusTooManyRequests, ErrTransientResponse, 7 * time.Second},
		{529, ErrTransientResponse, 0},
		{http.StatusRequestEntityTooLarge, ErrCapacityResponse, 0},
		{http.StatusRequestTimeout, ErrTransientResponse, 0},
		{http.StatusTeapot, ErrPermanentResponse, 0},
	} {
		header := http.Header{}
		if test.retry != 0 {
			header.Set("Retry-After", "7")
		}
		err := statusError(test.status, header)
		if !errors.Is(err, test.kind) {
			t.Errorf("status %d: got %v", test.status, err)
		}
		if delay, ok := RetryAfter(err); ok != (test.retry != 0) || ok && delay != test.retry {
			t.Errorf("status %d retry: %v %v", test.status, delay, ok)
		}
	}
}

func TestProviderErrorZeroValueIsSafe(t *testing.T) {
	var failure *ProviderError
	if failure.Error() == "" || failure.Unwrap() != nil {
		t.Fatal("nil ProviderError is unsafe")
	}
	if got := (&ProviderError{}).Error(); got == "" || (&ProviderError{}).Unwrap() != nil {
		t.Fatal("zero ProviderError is unsafe")
	}
}
