package daemonconn

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
)

func (c *Connection) BeginMailboxContainer(ctx context.Context, r store.MailboxContainerRequest) (store.MailboxContainer, error) {
	var out store.MailboxContainer
	var response *http.Response
	input := api.MailboxContainerInput{ID: r.ID, SHA256: r.SHA256, Size: r.Size, Format: r.Format}
	_, err := c.apiWithResponse(&response).BeginMailboxContainer(runtime.WithStreamingResponse(ctx), &apiclient.BeginMailboxContainerRequestOptions{Body: &input}, limitMailboxRequest)
	if err == nil {
		err = decodeMailbox(response, &out, 8<<20)
	}
	if err == nil && (out.ID != r.ID || out.SHA256 != r.SHA256 || out.Size != r.Size || out.Format != r.Format) {
		return out, integrityErrorf("mailbox container receipt differs from declared source")
	}
	return out, err
}
func (c *Connection) MailboxContainer(ctx context.Context, id string) (store.MailboxContainer, error) {
	var out store.MailboxContainer
	var response *http.Response
	_, err := c.apiWithResponse(&response).GetMailboxContainer(runtime.WithStreamingResponse(ctx), &apiclient.GetMailboxContainerRequestOptions{PathParams: &apiclient.GetMailboxContainerPath{ID: id}})
	if err == nil {
		err = decodeMailbox(response, &out, 8<<20)
	}
	if err == nil && out.ID != id {
		return out, integrityErrorf("mailbox container identity mismatch")
	}
	return out, err
}
func (c *Connection) UploadMailboxChunk(ctx context.Context, id string, index int, hash string, size int64, r io.Reader) error {
	if index < 0 || index >= store.MailboxMaxChunks || size < 1 || size > store.MailboxChunkBytes {
		return store.ErrMailboxInvalid
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).UploadMailboxChunk(runtime.WithStreamingResponse(ctx), &apiclient.UploadMailboxChunkRequestOptions{
		PathParams: &apiclient.UploadMailboxChunkPath{ID: id, Index: index},
		Header:     &apiclient.UploadMailboxChunkHeaders{XDocbankBlobHash: hash, XDocbankBlobSize: size},
	}, func(_ context.Context, request *http.Request) error {
		request.Body = io.NopCloser(io.LimitReader(r, size+1))
		return nil
	})
	if err != nil {
		return err
	}
	var chunk store.MailboxChunk
	if err = decodeMailbox(response, &chunk, 4096); err != nil {
		return err
	}
	if chunk.Index != index || chunk.Size != size || chunk.SHA256 != hash {
		return integrityErrorf("mailbox chunk receipt mismatch")
	}
	return nil
}
func (c *Connection) SealMailboxContainer(ctx context.Context, id string) (store.MailboxContainer, error) {
	var out store.MailboxContainer
	var response *http.Response
	_, err := c.apiWithResponse(&response).SealMailboxContainer(runtime.WithStreamingResponse(ctx), &apiclient.SealMailboxContainerRequestOptions{PathParams: &apiclient.SealMailboxContainerPath{ID: id}})
	if err == nil {
		err = decodeMailbox(response, &out, 8<<20)
	}
	if err == nil && (out.ID != id || out.State != "sealed") {
		return out, integrityErrorf("mailbox seal receipt mismatch")
	}
	return out, err
}
func (c *Connection) AbortMailboxContainer(ctx context.Context, id string) error {
	_, err := c.API().AbortMailboxContainer(ctx, &apiclient.AbortMailboxContainerRequestOptions{PathParams: &apiclient.AbortMailboxContainerPath{ID: id}})
	return err
}
func (c *Connection) PreviewMailbox(ctx context.Context, id, dialect string) (mailbox.Preview, error) {
	var out mailbox.Preview
	var response *http.Response
	_, err := c.apiWithResponse(&response).PreviewMailboxContainer(runtime.WithStreamingResponse(ctx), &apiclient.PreviewMailboxContainerRequestOptions{PathParams: &apiclient.PreviewMailboxContainerPath{ID: id}, Body: &apiclient.PreviewMailboxContainerBody{Dialect: &dialect}}, limitMailboxRequest)
	if err == nil {
		err = decodeMailbox(response, &out, 8<<20)
	}
	return out, err
}
func (c *Connection) BeginMailboxJob(ctx context.Context, r store.MailboxJobRequest) (store.MailboxJob, error) {
	var out store.MailboxJob
	var response *http.Response
	input := mailboxJobInput(r)
	_, err := c.apiWithResponse(&response).BeginMailboxJob(runtime.WithStreamingResponse(ctx), &apiclient.BeginMailboxJobRequestOptions{Body: &input}, limitMailboxRequest)
	if err == nil {
		err = decodeMailbox(response, &out, 8<<20)
	}
	if err == nil && (out.ID != r.ID || out.ContainerSHA256 != r.ContainerSHA256 || out.ContainerID != r.ContainerID) {
		return out, integrityErrorf("mailbox job receipt mismatch")
	}
	return out, err
}
func (c *Connection) MailboxJob(ctx context.Context, id string) (store.MailboxJob, error) {
	var out store.MailboxJob
	var response *http.Response
	_, err := c.apiWithResponse(&response).GetMailboxJob(runtime.WithStreamingResponse(ctx), &apiclient.GetMailboxJobRequestOptions{PathParams: &apiclient.GetMailboxJobPath{ID: id}})
	if err == nil {
		err = decodeMailbox(response, &out, 8<<20)
	}
	if err == nil && out.ID != id {
		return out, integrityErrorf("mailbox job identity mismatch")
	}
	return out, err
}
func (c *Connection) MailboxJobs(ctx context.Context, after string, limit int) ([]store.MailboxJob, error) {
	var out []store.MailboxJob
	if limit < 1 || limit > 100 {
		return nil, store.ErrMailboxLimit
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).ListMailboxJobs(runtime.WithStreamingResponse(ctx), &apiclient.ListMailboxJobsRequestOptions{Query: &apiclient.ListMailboxJobsQuery{After: &after, Limit: &limit}})
	if err == nil {
		err = decodeMailbox(response, &out, 8<<20)
	}
	return out, err
}
func (c *Connection) MailboxOccurrences(ctx context.Context, id string, after int64, limit int) ([]store.MailboxOccurrence, error) {
	var out []store.MailboxOccurrence
	if after < 0 || limit < 1 || limit > 100 {
		return nil, store.ErrMailboxLimit
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).MailboxOccurrences(runtime.WithStreamingResponse(ctx), &apiclient.MailboxOccurrencesRequestOptions{PathParams: &apiclient.MailboxOccurrencesPath{ID: id}, Query: &apiclient.MailboxOccurrencesQuery{After: &after, Limit: &limit}})
	if err == nil {
		err = decodeMailbox(response, &out, 8<<20)
	}
	return out, err
}
func (c *Connection) CancelMailboxJob(ctx context.Context, id string) error {
	_, err := c.API().CancelMailboxJob(ctx, &apiclient.CancelMailboxJobRequestOptions{PathParams: &apiclient.CancelMailboxJobPath{ID: id}})
	return err
}
func (c *Connection) ResumeMailboxJob(ctx context.Context, r store.MailboxJobRequest, continuation bool) (store.MailboxJob, error) {
	var out store.MailboxJob
	var response *http.Response
	_, err := c.apiWithResponse(&response).ResumeMailboxJob(runtime.WithStreamingResponse(ctx), &apiclient.ResumeMailboxJobRequestOptions{PathParams: &apiclient.ResumeMailboxJobPath{ID: r.ID}, Body: &apiclient.ResumeMailboxJobBody{Request: mailboxJobInput(r), Continuation: continuation}}, limitMailboxRequest)
	if err == nil {
		err = decodeMailbox(response, &out, 8<<20)
	}
	return out, err
}

func mailboxJobInput(r store.MailboxJobRequest) api.MailboxJobInput {
	return api.MailboxJobInput{ID: r.ID, ContainerID: r.ContainerID, ContainerSHA256: r.ContainerSHA256, Settings: api.MailboxSettingsInput(r.Settings)}
}

func (c *Connection) RegisterMailboxArchive(ctx context.Context, a store.MailboxArchive) error {
	var response *http.Response
	input := api.MailboxArchiveInput{ID: a.ID, Description: a.Description}
	_, err := c.apiWithResponse(&response).RegisterMailboxArchive(runtime.WithStreamingResponse(ctx), &apiclient.RegisterMailboxArchiveRequestOptions{Body: &input}, limitMailboxRequest)
	if err != nil {
		return err
	}
	return decodeMailbox(response, nil, 8<<20)
}
func (c *Connection) TransferMailboxEML(ctx context.Context, r store.MailboxTransferRequest, source io.Reader) (store.MailboxTransferReceipt, error) {
	var out store.MailboxTransferReceipt
	b, err := json.Marshal(r)
	if err != nil {
		return out, err
	}
	if len(b) > 24000 || r.Size < 1 || r.Size > 128<<20 {
		return out, store.ErrMailboxLimit
	}
	var response *http.Response
	_, err = c.apiWithResponse(&response).TransferMailboxEML(runtime.WithStreamingResponse(ctx), &apiclient.TransferMailboxEMLRequestOptions{Header: &apiclient.TransferMailboxEMLHeaders{XDocbankTransfer: base64.RawURLEncoding.EncodeToString(b)}}, func(_ context.Context, request *http.Request) error {
		request.Body = io.NopCloser(io.LimitReader(source, r.Size+1))
		return nil
	})
	if err == nil {
		err = decodeMailbox(response, &out, 1<<20)
	}
	if err == nil && out.Outcome != "tombstone" && (out.Target.SHA256 != r.SHA256 || out.Target.Size != r.Size || out.Request.ArchiveID != r.ArchiveID || out.Request.Reference != r.Reference) {
		return out, integrityErrorf("EML transfer receipt mismatch")
	}
	return out, err
}
