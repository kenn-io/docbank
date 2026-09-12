package client

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"io"
	"net/http"
	"net/url"
)

// WatchMailboxJob closes its response stream on cancellation or callback error.
// Disconnecting a watcher does not cancel the durable daemon job.
func (c *Client) WatchMailboxJob(ctx context.Context, id string, receive func(store.MailboxJob) error) error {
	request, err := c.emailRequest(ctx, "GET", mailboxBase+"/jobs/"+url.PathEscape(id)+"/events", nil)
	if err != nil {
		return err
	}
	response, err := c.hc.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return decodeError(response)
	}
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
