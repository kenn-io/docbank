package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

// ExternalPersonLinkRequest links an explicitly selected person, never a name match.
type ExternalPersonLinkRequest struct {
	System              string `json:"system" enum:"msgvault"`
	ArchiveID           string `json:"archive_id" minLength:"1" maxLength:"128"`
	UID                 string `json:"uid" minLength:"1" maxLength:"256"`
	UIDState            string `json:"uid_state" enum:"current,unlinked"`
	DisplayNameSnapshot string `json:"display_name_snapshot,omitzero" maxLength:"200"`
	LastSeenRevision    *int64 `json:"last_seen_revision,omitzero" minimum:"0"`
}

type ExternalPersonResolution struct {
	PersonID     string `json:"person_id" format:"uuid"`
	System       string `json:"system"`
	ArchiveID    string `json:"archive_id"`
	RequestedUID string `json:"requested_uid"`
	ResolvedUID  string `json:"resolved_uid"`
	UIDState     string `json:"uid_state"`
	Forwarded    bool   `json:"forwarded"`
}

func registerPersonExternalRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "linkPersonExternalIdentity", Method: http.MethodPut,
		Path:        "/api/v1/people/by-id/{person_id}/external-identities",
		Summary:     "Link or unlink a stable vCard identity on an explicitly selected person",
		Middlewares: huma.Middlewares{noStoreMediaReceipt},
	}, func(ctx context.Context, in *struct {
		PersonID string `path:"person_id" format:"uuid"`
		IfMatch  string `header:"If-Match"`
		Body     ExternalPersonLinkRequest
	}) (*struct {
		ETag string `header:"ETag"`
		Body PersonExternalIdentity
	}, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var saved store.PersonExternalIdentity
		err = g.mutate(ctx, func() error {
			var callErr error
			saved, callErr = d.Store.LinkExternalIdentity(ctx, store.PersonExternalIdentity{
				PersonID: in.PersonID, System: in.Body.System, ArchiveID: in.Body.ArchiveID,
				UID: in.Body.UID, UIDKind: "vcard_uid", UIDState: in.Body.UIDState,
				DisplayNameSnapshot: in.Body.DisplayNameSnapshot, LastSeenRevision: in.Body.LastSeenRevision,
			}, revision)
			return FromStoreError(callErr)
		})
		if err != nil {
			return nil, err
		}
		return &struct {
			ETag string `header:"ETag"`
			Body PersonExternalIdentity
		}{ETag: revisionETag(revision + 1), Body: PersonExternalIdentity{
			System: saved.System, ArchiveID: saved.ArchiveID, UID: saved.UID,
			UIDKind: saved.UIDKind, UIDState: saved.UIDState, LastSeenRevision: saved.LastSeenRevision,
			DisplayNameSnapshot: saved.DisplayNameSnapshot, LinkedAt: saved.LinkedAt, UpdatedAt: saved.UpdatedAt,
		}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "resolveExternalPerson", Method: http.MethodGet,
		Path: "/api/v1/people/resolve-external", Summary: "Resolve an archive-scoped stable person UID",
		Middlewares: huma.Middlewares{noStoreMediaReceipt},
	}, func(ctx context.Context, in *struct {
		System    string `query:"system" required:"true" enum:"msgvault"`
		ArchiveID string `query:"archive_id" required:"true" minLength:"1" maxLength:"128"`
		UID       string `query:"uid" required:"true" minLength:"1" maxLength:"256"`
	}) (*struct{ Body ExternalPersonResolution }, error) {
		resolved, err := d.Store.ResolveExternalPersonUID(ctx, in.System, in.ArchiveID, in.UID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &struct{ Body ExternalPersonResolution }{Body: ExternalPersonResolution{
			PersonID: resolved.PersonID, System: resolved.System, ArchiveID: resolved.ArchiveID,
			RequestedUID: resolved.RequestedUID, ResolvedUID: resolved.ResolvedUID,
			UIDState: resolved.UIDState, Forwarded: resolved.Forwarded,
		}}, nil
	})
}
