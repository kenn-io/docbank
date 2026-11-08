package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

// noteSetupGET uses the existing ownership-proven connection while the new
// route declarations await the repository's remote generation workflow.
func noteSetupGET[T any](ctx context.Context, c *Connection, path string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, errors.New("cannot build note setup request")
	}
	req.Header.Set("X-Api-Key", c.key)
	resp, err := c.hc.Do(req) // #nosec G704 -- Fixed relative routes on the existing proven daemon connection.
	if err != nil {
		return nil, classifyRequestFailure(resp, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, decodeError(resp)
	}
	return readNotePublicationResponse[T](resp, nil, 64<<10)
}

// NoteLocalTextPlan reads the daemon's existing fixed-plan preview. Reading a
// plan never grants consent or starts processing.
func (c *Connection) NoteLocalTextPlan(ctx context.Context) (api.NoteLocalTextPlan, error) {
	plan, err := noteSetupGET[api.NoteLocalTextPlan](ctx, c, "/api/v1/notes/local-text-plan")
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
	query := url.Values{"source_ref": {sourceRef}}
	path := fmt.Sprintf("/api/v1/note-publications/%s/sources/current?%s", url.PathEscape(name), query.Encode())
	snapshot, err := noteSetupGET[api.NotePublicationSourceSnapshot](ctx, c, path)
	if err != nil {
		return api.NotePublicationSourceSnapshot{}, err
	}
	if snapshot == nil || store.ValidatePushRunID(snapshot.DatabaseEpoch) != nil ||
		!validNoteSource(snapshot.Source, name) || snapshot.Source.Identity.SourceRef != sourceRef {
		return api.NotePublicationSourceSnapshot{}, badNoteResponse()
	}
	return *snapshot, nil
}
