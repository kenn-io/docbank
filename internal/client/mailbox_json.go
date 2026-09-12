package client

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
)

func (c *Client) doMailbox(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		if len(data) > 1<<20 {
			return integrityErrorf("mailbox request exceeds byte limit")
		}
		body = bytes.NewReader(data)
	}
	request, err := c.emailRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.hc.Do(request)
	if err != nil {
		return &transportError{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeError(response)
	}
	// A page contains at most 100 catalog records of at most 64 KiB each.
	const limit = 8 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return integrityErrorf("mailbox response exceeds byte limit")
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(data, output, json.RejectUnknownMembers(true))
}
