// Package cohereapi contains provider-neutral mechanics shared by the fixed
// Cohere hosted adapters. It does not own operation identity, authorization,
// profiles, receipts, or adapter-specific error types.
package cohereapi

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document/providerhttp"
)

var ErrResponseRead = errors.New("cohere API response read failed")

type StatusKind uint8

const (
	StatusPermanent StatusKind = iota + 1
	StatusTransient
	StatusCapacity
)

type StatusResult struct {
	Kind       StatusKind
	RetryDelay time.Duration
	RetrySet   bool
}

func ClassifyStatus(status int, retryAfter string, now time.Time) StatusResult {
	result := StatusResult{Kind: StatusPermanent}
	switch {
	case status == http.StatusRequestEntityTooLarge:
		result.Kind = StatusCapacity
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 && status <= 599:
		result.Kind = StatusTransient
		result.RetryDelay, result.RetrySet = providerhttp.ParseRetryAfter(retryAfter, now)
	}
	return result
}

func ValidToken(value string, maximumBytes int) bool {
	return maximumBytes > 0 && value != "" && len(value) <= maximumBytes && utf8.ValidString(value) &&
		value == strings.TrimSpace(value) && strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsControl(character) || unicode.IsSpace(character)
	}) < 0
}

func IsJSONContentType(value string) bool {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/json" {
		return false
	}
	if len(parameters) == 0 {
		return true
	}
	charset, ok := parameters["charset"]
	return len(parameters) == 1 && ok && strings.EqualFold(charset, "utf-8")
}

type ReadOutcome uint8

const (
	ReadOK ReadOutcome = iota
	ReadTransient
	ReadCapacity
	ReadCanceled
)

func ReadBounded(ctx context.Context, reader io.Reader, maximum int64) ([]byte, ReadOutcome, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if contextErr := ctx.Err(); contextErr != nil {
		clear(body)
		return nil, ReadCanceled, contextErr
	}
	if err != nil {
		clear(body)
		return nil, ReadTransient, ErrResponseRead
	}
	if int64(len(body)) > maximum {
		clear(body)
		return nil, ReadCapacity, nil
	}
	return body, ReadOK, nil
}
