package client

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
)

const mailboxBase = "/api/v1/mailbox"

func (c *Client) BeginMailboxContainer(ctx context.Context, r store.MailboxContainerRequest) (store.MailboxContainer, error) {
	var out store.MailboxContainer
	err := c.doMailbox(ctx, "POST", mailboxBase+"/containers", r, &out)
	if err == nil && (out.ID != r.ID || out.SHA256 != r.SHA256 || out.Size != r.Size || out.Format != r.Format) {
		return out, integrityErrorf("mailbox container receipt differs from declared source")
	}
	return out, err
}
func (c *Client) MailboxContainer(ctx context.Context, id string) (store.MailboxContainer, error) {
	var out store.MailboxContainer
	err := c.doMailbox(ctx, "GET", mailboxBase+"/containers/"+url.PathEscape(id), nil, &out)
	if err == nil && out.ID != id {
		return out, integrityErrorf("mailbox container identity mismatch")
	}
	return out, err
}
func (c *Client) UploadMailboxChunk(ctx context.Context, id string, index int, hash string, size int64, r io.Reader) error {
	if index < 0 || index >= store.MailboxMaxChunks || size < 1 || size > store.MailboxChunkBytes {
		return store.ErrMailboxInvalid
	}
	req, err := c.emailRequest(ctx, "PUT", mailboxBase+"/containers/"+url.PathEscape(id)+"/chunks/"+strconv.Itoa(index), io.LimitReader(r, size+1))
	if err != nil {
		return err
	}
	req.Header.Set(api.BlobHashHeader, hash)
	req.Header.Set(api.BlobSizeHeader, strconv.FormatInt(size, 10))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return decodeError(resp)
	}
	var chunk store.MailboxChunk
	if err = json.UnmarshalRead(io.LimitReader(resp.Body, 4096), &chunk); err != nil {
		return err
	}
	if chunk.Index != index || chunk.Size != size || chunk.SHA256 != hash {
		return integrityErrorf("mailbox chunk receipt mismatch")
	}
	return nil
}
func (c *Client) SealMailboxContainer(ctx context.Context, id string) (store.MailboxContainer, error) {
	var out store.MailboxContainer
	err := c.doMailbox(ctx, "POST", mailboxBase+"/containers/"+url.PathEscape(id)+"/seal", struct{}{}, &out)
	if err == nil && (out.ID != id || out.State != "sealed") {
		return out, integrityErrorf("mailbox seal receipt mismatch")
	}
	return out, err
}
func (c *Client) AbortMailboxContainer(ctx context.Context, id string) error {
	return c.doMailbox(ctx, "DELETE", mailboxBase+"/containers/"+url.PathEscape(id), nil, nil)
}
func (c *Client) PreviewMailbox(ctx context.Context, id, dialect string) (mailbox.Preview, error) {
	var out mailbox.Preview
	err := c.doMailbox(ctx, "POST", mailboxBase+"/containers/"+url.PathEscape(id)+"/preview", struct {
		Dialect string `json:"dialect"`
	}{dialect}, &out)
	return out, err
}
func (c *Client) BeginMailboxJob(ctx context.Context, r store.MailboxJobRequest) (store.MailboxJob, error) {
	var out store.MailboxJob
	err := c.doMailbox(ctx, "POST", mailboxBase+"/jobs", r, &out)
	if err == nil && (out.ID != r.ID || out.ContainerSHA256 != r.ContainerSHA256 || out.ContainerID != r.ContainerID) {
		return out, integrityErrorf("mailbox job receipt mismatch")
	}
	return out, err
}
func (c *Client) MailboxJob(ctx context.Context, id string) (store.MailboxJob, error) {
	var out store.MailboxJob
	err := c.doMailbox(ctx, "GET", mailboxBase+"/jobs/"+url.PathEscape(id), nil, &out)
	if err == nil && out.ID != id {
		return out, integrityErrorf("mailbox job identity mismatch")
	}
	return out, err
}
func (c *Client) MailboxJobs(ctx context.Context, after string, limit int) ([]store.MailboxJob, error) {
	var out []store.MailboxJob
	if limit < 1 || limit > 100 {
		return nil, store.ErrMailboxLimit
	}
	err := c.doMailbox(ctx, "GET", mailboxBase+"/jobs?after="+url.QueryEscape(after)+"&limit="+strconv.Itoa(limit), nil, &out)
	return out, err
}
func (c *Client) MailboxOccurrences(ctx context.Context, id string, after int64, limit int) ([]store.MailboxOccurrence, error) {
	var out []store.MailboxOccurrence
	if after < 0 || limit < 1 || limit > 100 {
		return nil, store.ErrMailboxLimit
	}
	err := c.doMailbox(ctx, "GET", mailboxBase+"/jobs/"+url.PathEscape(id)+"/occurrences?after="+strconv.FormatInt(after, 10)+"&limit="+strconv.Itoa(limit), nil, &out)
	return out, err
}
func (c *Client) CancelMailboxJob(ctx context.Context, id string) error {
	return c.doMailbox(ctx, "POST", mailboxBase+"/jobs/"+url.PathEscape(id)+"/cancel", struct{}{}, nil)
}
func (c *Client) ResumeMailboxJob(ctx context.Context, r store.MailboxJobRequest, continuation bool) (store.MailboxJob, error) {
	var out store.MailboxJob
	err := c.doMailbox(ctx, "POST", mailboxBase+"/jobs/"+url.PathEscape(r.ID)+"/resume", struct {
		Request      store.MailboxJobRequest `json:"request"`
		Continuation bool                    `json:"continuation"`
	}{r, continuation}, &out)
	return out, err
}
func (c *Client) RegisterMailboxArchive(ctx context.Context, a store.MailboxArchive) error {
	return c.doMailbox(ctx, "POST", mailboxBase+"/archives", a, nil)
}
func (c *Client) TransferMailboxEML(ctx context.Context, r store.MailboxTransferRequest, source io.Reader) (store.MailboxTransferReceipt, error) {
	var out store.MailboxTransferReceipt
	b, err := json.Marshal(r)
	if err != nil {
		return out, err
	}
	if len(b) > 24000 || r.Size < 1 || r.Size > 128<<20 {
		return out, store.ErrMailboxLimit
	}
	req, err := c.emailRequest(ctx, "POST", mailboxBase+"/transfers", io.LimitReader(source, r.Size+1))
	if err != nil {
		return out, err
	}
	req.Header.Set("X-Docbank-Transfer", base64.RawURLEncoding.EncodeToString(b))
	req.Header.Set("Content-Type", "message/rfc822")
	resp, err := c.hc.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return out, decodeError(resp)
	}
	err = json.UnmarshalRead(io.LimitReader(resp.Body, 1<<20), &out)
	if err == nil && out.Outcome != "tombstone" && (out.Target.SHA256 != r.SHA256 || out.Target.Size != r.Size || out.Request.ArchiveID != r.ArchiveID || out.Request.Reference != r.Reference) {
		return out, integrityErrorf("EML transfer receipt mismatch")
	}
	return out, err
}
