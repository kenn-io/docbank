package daemonconn

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

// ChangeCursorExpiredError carries the cursor to save before a fresh scan.
type ChangeCursorExpiredError struct{ Cursor string }

func (e *ChangeCursorExpiredError) Error() string {
	return "vault change cursor expired; rescan, then resume with --after " + e.Cursor
}
func (e *ChangeCursorExpiredError) Unwrap() error { return store.ErrChangeCursorExpired }

// WatchChanges reads committed changes until drained, or until cancellation
// when following. A callback error closes the stream immediately.
func (c *Connection) WatchChanges(ctx context.Context, after string, follow bool, receive func(store.VaultChange) error) (string, error) {
	var response *http.Response
	_, err := c.apiWithResponse(&response).VaultChanges(runtime.WithStreamingResponse(ctx), &apiclient.VaultChangesRequestOptions{Query: &apiclient.VaultChangesQuery{After: &after, Follow: &follow}})
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 128<<10)
	for scanner.Scan() {
		var event api.ChangeEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return "", err
		}
		if event.Type == "error" && event.Error != nil {
			return "", apiProblemError(*event.Error)
		}
		if event.Type == "result" {
			return event.Cursor, nil
		}
		if event.Type != "change" || event.Change == nil {
			return "", integrityErrorf("invalid vault change record")
		}
		if receive != nil {
			if err := receive(*event.Change); err != nil {
				return "", err
			}
		}
	}
	return "", errors.Join(io.ErrUnexpectedEOF, scanner.Err())
}
