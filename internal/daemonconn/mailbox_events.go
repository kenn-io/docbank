package daemonconn

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
	"io"
	"net/http"
)

// WatchMailboxJob closes its response stream on cancellation or callback error.
// Disconnecting a watcher does not cancel the durable daemon job.
func (c *Connection) WatchMailboxJob(ctx context.Context, id string, receive func(store.MailboxJob) error) error {
	var response *http.Response
	_, err := c.apiWithResponse(&response).MailboxEvents(runtime.WithStreamingResponse(ctx), &apiclient.MailboxEventsRequestOptions{PathParams: &apiclient.MailboxEventsPath{ID: id}})
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 128<<10)
	for scanner.Scan() {
		var event api.MailboxEvent
		if err = json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if event.Type == "error" {
			return fmt.Errorf("mailbox event: %s", event.Error)
		}
		if event.Job == nil || event.Job.ID != id || (event.Type != "progress" && event.Type != "result") {
			return integrityErrorf("mailbox event identity or type mismatch")
		}
		if receive != nil {
			if err = receive(*event.Job); err != nil {
				return err
			}
		}
		if event.Type == "result" {
			return nil
		}
	}
	return errors.Join(io.ErrUnexpectedEOF, scanner.Err())
}
