package embedclient

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidVector reports that the provider returned a vector that failed
// validation: wrong width, a null or non-finite component, or a zero norm.
// A *VectorError carries the input it belongs to.
var ErrInvalidVector = errors.New("embed vector is invalid")

// ErrResponseTooLarge reports a response body above Transport.MaxResponseBytes.
var ErrResponseTooLarge = errors.New("embed response exceeds the configured cap")

// VectorError is an invalid vector for one input. Index is the position in
// the inputs passed to Embed, EmbedTexts, or the EncodeFunc call. It matches
// ErrInvalidVector with errors.Is.
type VectorError struct {
	Index int
	Err   error
}

func (e *VectorError) Error() string {
	return fmt.Sprintf("embed vector %d: %v", e.Index, e.Err)
}

func (e *VectorError) Unwrap() []error { return []error{ErrInvalidVector, e.Err} }

// TransportError is a request that did not produce an HTTP response, such as
// a DNS, connection, or TLS failure. Its message omits the cause, because
// transport errors can name internal hosts. Unwrap returns the cause, so
// errors.As and errors.Is still reach it. Cancellation and deadline errors
// are returned wrapped in TransportError too.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return "embed request failed" }

func (e *TransportError) Unwrap() error { return e.Err }

// Reason is the kind of failure an embedding endpoint reported. Kit derives
// it from the status and the provider's error body, then discards the body.
type Reason int

const (
	// ReasonUnknown is a failure Kit could not classify. Callers should not
	// treat it as a rejection of one input.
	ReasonUnknown Reason = iota
	// ReasonInputTooLong is an input over the model's token or context limit.
	ReasonInputTooLong
	// ReasonContentPolicy is an input the provider refused under its content
	// policy.
	ReasonContentPolicy
	// ReasonInvalidRequest is a request the endpoint can never accept as
	// sent, whatever the input: an unknown model, a wrong route, or an
	// unsupported field. It points at configuration.
	ReasonInvalidRequest
	// ReasonCredentials is a refused key or permission (401 or 403).
	ReasonCredentials
	// ReasonRateLimited is a 429.
	ReasonRateLimited
	// ReasonUnavailable is a timeout or server failure (408 or 5xx).
	ReasonUnavailable
)

func (r Reason) String() string {
	switch r {
	case ReasonInputTooLong:
		return "input too long"
	case ReasonContentPolicy:
		return "content refused by policy"
	case ReasonInvalidRequest:
		return "invalid request"
	case ReasonCredentials:
		return "credentials rejected"
	case ReasonRateLimited:
		return "rate limited"
	case ReasonUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// APIError is a non-2xx embedding response. Reason classifies it; the
// provider body is read only to classify and is never kept or echoed.
type APIError struct {
	StatusCode int
	RetryAfter time.Duration
	Reason     Reason
}

func (e *APIError) Error() string {
	return fmt.Sprintf("embed endpoint returned %d (%s)", e.StatusCode, e.Reason)
}

// InputRejected reports that the endpoint refused this input itself: it is
// too long or refused by policy. The same input will fail again, and other
// inputs may succeed. A 400 Kit cannot attribute to the input is not an input
// rejection.
func (e *APIError) InputRejected() bool {
	return e.Reason == ReasonInputTooLong || e.Reason == ReasonContentPolicy
}

// CredentialsRejected reports that the key or the permission was refused.
// That is not a reason to skip one document.
func (e *APIError) CredentialsRejected() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// Retryable reports a response that may succeed later: 408, 429, or a 5xx.
// Check RetryAfter for the delay the provider asked for.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusRequestTimeout || e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// maxErrorBody bounds how much of a failed response Kit reads to classify it.
const maxErrorBody = 4096

// newAPIError reads up to maxErrorBody bytes of a non-2xx response to
// classify it. The body is not kept.
func newAPIError(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return &APIError{
		StatusCode: resp.StatusCode,
		RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		Reason:     classifyFailure(resp.StatusCode, body),
	}
}

func retryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(header)
	if err != nil {
		return 0
	}
	if delay := time.Until(when); delay > 0 {
		return delay
	}
	return 0
}

func classifyTransport(err error) error {
	if errors.Is(err, ErrOrigin) {
		return ErrOrigin
	}
	return &TransportError{Err: err}
}

// remapVectorIndex rewrites the index of a VectorError in err with index.
// Each request numbers its own inputs; callers see their own positions.
func remapVectorIndex(err error, index func(int) int) error {
	if vectorErr, ok := errors.AsType[*VectorError](err); ok {
		vectorErr.Index = index(vectorErr.Index)
	}
	return err
}
