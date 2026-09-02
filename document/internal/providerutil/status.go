package providerutil

import (
	"net/http"
	"strconv"
	"time"

	"go.kenn.io/docbank/document"
)

// Stage names the role of one HTTP exchange in a rendering.
type Stage string

const (
	// StageSubmission creates provider work; its failures can leave a job
	// behind that Docbank cannot see.
	StageSubmission Stage = "submission"
	// StageJob addresses work the provider already acknowledged by ID.
	StageJob Stage = "job"
	// StageResult is one synchronous exchange whose 2xx body is the final
	// result. A failure before a usable response is ambiguous, but a 2xx body
	// Docbank cannot accept is terminal malformed evidence: resubmitting the
	// same bytes returns the same result.
	StageResult Stage = "result"
)

// StatusClass maps one non-2xx HTTP status to its rendition error class and
// private cause message. Every adapter uses the same table:
//
//	401, 403       authentication
//	404, 410       unknown_job for a known job; malformed_evidence on
//	               submission, because the fixed route itself is missing
//	400, 413, 422  policy_rejected on submission (the provider refused the
//	               request as Docbank sent it); malformed_evidence otherwise
//	415            unsupported_input on submission; malformed_evidence otherwise
//	429            rate_limited, carrying the Retry-After hint
//	503, 507       capacity, carrying the Retry-After hint
//	408, other 5xx transient
//	anything else  malformed_evidence
//
// Outcomes that are not HTTP statuses follow the same rules everywhere:
//
//   - A submission that fails with 408, any 5xx, a transport error, or an
//     unusable 2xx body is wrapped as ambiguous_submission (see
//     Provider.StatusError and Executor.Do): the provider may have accepted
//     the job before failing. A 4xx refusal is never ambiguous. A synchronous
//     result exchange (StageResult) differs only for a 2xx body that is
//     malformed, which stays terminal.
//   - Polling exhaustion and a total timeout after the job is known are
//     ambiguous_submission wrapping a capacity cause: the job may still finish
//     remotely, so a blind resubmission would duplicate it.
//   - A total timeout before the job is known is capacity.
//   - A provider-side conversion failure ("failed" or "failure" job states)
//     is malformed_evidence: the provider accepted the input but produced no
//     evidence, and retrying the same bytes cannot help.
//   - Caller-context cancellation is canceled, wrapping the context error so
//     errors.Is still matches context.Canceled or context.DeadlineExceeded.
//   - Authorization expiry is policy_rejected.
func (provider Provider) StatusClass(stage Stage, status int) (document.RenditionErrorCode, string) {
	name := string(provider)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return document.RenditionErrorAuthentication, name + " authentication failed"
	case http.StatusNotFound, http.StatusGone:
		if stage == StageJob {
			return document.RenditionErrorUnknownJob, name + " job is unknown or expired"
		}
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		if stage != StageJob {
			return document.RenditionErrorPolicyRejected, name + " rejected the submitted input"
		}
	case http.StatusUnsupportedMediaType:
		if stage != StageJob {
			return document.RenditionErrorUnsupportedInput, name + " does not support the submitted input"
		}
	case http.StatusTooManyRequests:
		return document.RenditionErrorRateLimited, name + " rate limit"
	case http.StatusServiceUnavailable, http.StatusInsufficientStorage:
		return document.RenditionErrorCapacity, name + " capacity is temporarily unavailable"
	case http.StatusRequestTimeout:
		return document.RenditionErrorTransient, name + " is temporarily unavailable"
	}
	if status >= http.StatusInternalServerError && status < 600 {
		return document.RenditionErrorTransient, name + " is temporarily unavailable"
	}
	return document.RenditionErrorMalformedEvidence, name + " returned unexpected HTTP status " + strconv.Itoa(status)
}

// StatusError classifies one non-2xx response per StatusClass, attaching the
// provider retry hint to retryable classes and wrapping ambiguous submissions.
func (provider Provider) StatusError(stage Stage, status int, retryAfter time.Duration, cause error) error {
	code, message := provider.StatusClass(stage, status)
	if !retryableCode(code) {
		retryAfter = 0
	}
	err := ClassifiedError(string(provider), code, message, retryAfter, cause)
	if stage != StageJob && ambiguousStatus(status) {
		return provider.AmbiguousSubmission(err)
	}
	return err
}

func ambiguousStatus(status int) bool {
	return status == http.StatusRequestTimeout || status >= http.StatusInternalServerError && status < 600
}

func retryableCode(code document.RenditionErrorCode) bool {
	switch code {
	case document.RenditionErrorCapacity, document.RenditionErrorRateLimited, document.RenditionErrorTransient:
		return true
	default:
		return false
	}
}
