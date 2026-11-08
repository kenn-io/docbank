package daemonconn

import (
	"context"
	"errors"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

// NoteLocalTextPlan reads the daemon's existing fixed-plan preview. Reading a
// plan never grants consent or starts processing.
func (c *Connection) NoteLocalTextPlan(ctx context.Context) (api.NoteLocalTextPlan, error) {
	var response *http.Response
	_, callErr := c.apiWithResponse(&response).GetNoteLocalTextPlan(runtime.WithStreamingResponse(ctx))
	plan, err := readNotePublicationResponse[api.NoteLocalTextPlan](response, callErr, 64<<10)
	if err != nil {
		return api.NoteLocalTextPlan{}, err
	}
	if plan == nil {
		return api.NoteLocalTextPlan{}, badNoteResponse()
	}
	return *plan, nil
}

// NotePublicationSource reads one exact current binding and its archive head.
func (c *Connection) NotePublicationSource(ctx context.Context, name, sourceRef string) (api.NotePublicationSourceSnapshot, error) {
	if store.ValidatePushName(name) != nil || store.ValidatePushRef(sourceRef) != nil {
		return api.NotePublicationSourceSnapshot{}, errors.New("invalid note publication source")
	}
	var response *http.Response
	_, callErr := c.apiWithResponse(&response).GetNotePublicationSource(runtime.WithStreamingResponse(ctx),
		&apiclient.GetNotePublicationSourceRequestOptions{PathParams: &apiclient.GetNotePublicationSourcePath{Name: name}, Query: &apiclient.GetNotePublicationSourceQuery{SourceRef: sourceRef}})
	snapshot, err := readNotePublicationResponse[api.NotePublicationSourceSnapshot](response, callErr, 64<<10)
	if err != nil {
		return api.NotePublicationSourceSnapshot{}, err
	}
	if snapshot == nil || store.ValidatePushRunID(snapshot.DatabaseEpoch) != nil ||
		!validNoteSource(snapshot.Source, name) || snapshot.Source.Identity.SourceRef != sourceRef {
		return api.NotePublicationSourceSnapshot{}, badNoteResponse()
	}
	return *snapshot, nil
}
