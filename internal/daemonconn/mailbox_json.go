package daemonconn

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
)

func decodeMailbox(response *http.Response, output any, limit int64) error {
	defer func() { _ = response.Body.Close() }()
	// A page contains at most 100 catalog records of at most 64 KiB each.
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return &responseDecodeError{err: err}
	}
	if int64(len(data)) > limit {
		return &responseDecodeError{err: integrityErrorf("mailbox response exceeds byte limit")}
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(data, output, json.RejectUnknownMembers(true)); err != nil {
		return &responseDecodeError{err: err}
	}
	return nil
}

func limitMailboxRequest(_ context.Context, request *http.Request) error {
	if request.ContentLength > 1<<20 {
		return integrityErrorf("mailbox request exceeds byte limit")
	}
	return nil
}
