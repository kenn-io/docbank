package daemonconn

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

// AcceptLocalPublishedNote authorizes a later local upload against one exact
// observed archive head. The response must echo all of that grant's bindings.
func (c *Connection) AcceptLocalPublishedNote(ctx context.Context, request api.NotePublicationAcceptLocalRequest) (api.NotePublicationRecoveryGrant, error) {
	if store.ValidatePushName(request.Name) != nil || store.ValidatePushRef(request.SourceRef) != nil || request.ExpectedSourceRevision < 1 {
		return api.NotePublicationRecoveryGrant{}, errors.New("invalid local note recovery request")
	}
	for _, id := range []string{request.DatabaseEpoch, request.ExpectedHeadVersionID, request.OperationID} {
		if store.ValidatePushRunID(id) != nil {
			return api.NotePublicationRecoveryGrant{}, errors.New("invalid local note recovery request")
		}
	}
	var response *http.Response
	grant, err := c.apiWithResponse(&response).AcceptLocalPublishedNote(ctx, &apiclient.AcceptLocalPublishedNoteRequestOptions{
		PathParams: &apiclient.AcceptLocalPublishedNotePath{Name: request.Name}, Body: &request,
	})
	if err != nil {
		return api.NotePublicationRecoveryGrant{}, mutationRequestError(response, err)
	}
	if grant == nil || grant.SourceRef != request.SourceRef || grant.ExpectedHeadVersionID != request.ExpectedHeadVersionID || grant.SourceRevision != request.ExpectedSourceRevision {
		return api.NotePublicationRecoveryGrant{}, &responseDecodeError{err: errors.New("note recovery response does not match the requested grant")}
	}
	return *grant, nil
}
