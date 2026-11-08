package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"maps"
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

const notePublicationBase = "/api/v1/note-publications/{name}"
const noteRevisionFormat = "int64"

// NoteLocalTextPlan describes the fixed local-only policy and the exact
// ordinary consent request required to enable it.
type NoteLocalTextPlan struct {
	Plan           ProcessingPlan                    `json:"plan"`
	ConsentRequest document.ProcessingConsentRequest `json:"consent_request"`
}

func registerNotePublicationRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "getNoteLocalTextPlan", Method: http.MethodGet,
		Path: "/api/v1/notes/local-text-plan", Summary: "Preview local note text indexing consent",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body NoteLocalTextPlan }, error) {
		if d.Processing == nil {
			return nil, processingUnavailable()
		}
		plan, consent, err := d.Processing.NoteLocalTextPlan(ctx)
		if err != nil {
			return nil, fromProcessingError(err)
		}
		return &struct{ Body NoteLocalTextPlan }{NoteLocalTextPlan{Plan: fromProcessingPlan(plan), ConsentRequest: consent}}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "getNotePublicationSource", Method: http.MethodGet, Path: notePublicationBase + "/sources/current",
		Summary: "Read an accepted source and current archive head without beginning an inventory",
	}, func(ctx context.Context, in *struct {
		Name      string `path:"name" maxLength:"64"`
		SourceRef string `query:"source_ref" required:"true" maxLength:"4096"`
	}) (*struct{ Body NotePublicationSourceSnapshot }, error) {
		if err := validateNoteRoute(d, in.Name); err != nil {
			return nil, err
		}
		if store.ValidatePushRef(in.SourceRef) != nil {
			return nil, invalidNoteRequest()
		}
		snapshot, err := d.Store.NotePublicationSource(ctx, in.Name, in.SourceRef)
		if err != nil {
			return nil, notePublicationError(err)
		}
		return &struct{ Body NotePublicationSourceSnapshot }{NotePublicationSourceSnapshot{
			DatabaseEpoch: snapshot.DatabaseEpoch, Source: fromPublishedNoteSource(snapshot.Source),
		}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "beginNotePublicationScan", Method: http.MethodPost, Path: notePublicationBase + "/scans",
		Summary: "Begin or replay a managed note inventory", MaxBodyBytes: 16 << 10,
	}, func(ctx context.Context, in *struct {
		Name    string `path:"name" maxLength:"64"`
		Body    NotePublicationBeginRequest
		RawBody []byte
	}) (*struct{ Body NotePublicationScan }, error) {
		if err := strictNoteBody(in.RawBody, &in.Body); err != nil {
			return nil, err
		}
		if err := notePathBinding(in.Name, "", in.Body.Name, ""); err != nil {
			return nil, err
		}
		if err := validateNoteRoute(d, in.Name, in.Body.OperationID); err != nil {
			return nil, err
		}
		if err := store.ValidatePushDestination(in.Body.Destination); err != nil {
			return nil, invalidNoteRequest()
		}
		in.Body.Name = in.Name
		var result store.NotePublicationScan
		err := g.mutate(ctx, func() error {
			var err error
			result, err = d.Store.BeginNotePublicationScan(ctx, store.NotePublicationBeginRequest(in.Body))
			return err
		})
		if err != nil {
			return nil, notePublicationError(err)
		}
		return &struct{ Body NotePublicationScan }{NotePublicationScan(result)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listNotePublicationSources", Method: http.MethodGet, Path: notePublicationBase + "/sources",
		Summary: "Read exact accepted sources within one inventory epoch",
	}, func(ctx context.Context, in *struct {
		Name   string `path:"name" maxLength:"64"`
		ScanID string `query:"scan_id" required:"true" maxLength:"36"`
		After  string `query:"after" maxLength:"4096"`
		Limit  int    `query:"limit" minimum:"1" maximum:"1000" default:"1000"`
	}) (*struct{ Body NotePublicationSourcesPage }, error) {
		if err := validateNoteRoute(d, in.Name, in.ScanID); err != nil {
			return nil, err
		}
		if in.After != "" && store.ValidatePushRef(in.After) != nil {
			return nil, invalidNoteRequest()
		}
		page, err := d.Store.NotePublicationSources(ctx, store.NotePublicationSourcesRequest{Name: in.Name, ScanID: in.ScanID, After: in.After, Limit: in.Limit})
		if err != nil {
			return nil, notePublicationError(err)
		}
		result := NotePublicationSourcesPage{Scan: NotePublicationScan(page.Scan), Items: make([]NotePublicationSource, 0, len(page.Items)), Next: page.Next}
		for _, source := range page.Items {
			result.Items = append(result.Items, fromPublishedNoteSource(source))
		}
		return &struct{ Body NotePublicationSourcesPage }{result}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "acknowledgePublishedNotes", Method: http.MethodPost, Path: notePublicationBase + "/scans/{scan_id}/seen",
		Summary: "Acknowledge exact unchanged sources atomically", MaxBodyBytes: 1 << 20,
	}, func(ctx context.Context, in *struct {
		Name    string `path:"name" maxLength:"64"`
		ScanID  string `path:"scan_id" maxLength:"36"`
		Body    NotePublicationSeenRequest
		RawBody []byte
	}) (*struct{ Body NotePublicationAck }, error) {
		if err := strictNoteBody(in.RawBody, &in.Body); err != nil {
			return nil, err
		}
		if err := notePathBinding(in.Name, in.ScanID, in.Body.Name, in.Body.ScanID); err != nil {
			return nil, err
		}
		if err := validateNoteRoute(d, in.Name, in.ScanID); err != nil {
			return nil, err
		}
		request := store.NotePublicationSeenRequest{Name: in.Name, ScanID: in.ScanID, Bindings: make([]store.PublishedNoteIdentity, 0, len(in.Body.Bindings))}
		for _, binding := range in.Body.Bindings {
			id := store.PublishedNoteIdentity(binding)
			if err := store.ValidatePublishedNoteSource(store.NotePublicationSource{Identity: id, Presence: "present", Availability: "retained"}); err != nil {
				return nil, invalidNoteRequest()
			}
			request.Bindings = append(request.Bindings, id)
		}
		var ack store.NotePublicationAck
		err := g.mutate(ctx, func() error {
			var err error
			ack, err = d.Store.AcknowledgePublishedNotes(ctx, request)
			return err
		})
		if err != nil {
			return nil, notePublicationError(err)
		}
		result := NotePublicationAck{Scan: NotePublicationScan(ack.Scan), Sources: make([]PublishedNoteIdentity, 0, len(ack.Sources))}
		for _, id := range ack.Sources {
			result.Sources = append(result.Sources, PublishedNoteIdentity(id))
		}
		return &struct{ Body NotePublicationAck }{result}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "completeNotePublicationScan", Method: http.MethodPost, Path: notePublicationBase + "/scans/{scan_id}/complete",
		Summary: "Seal a successful inventory and retire unseen sources", MaxBodyBytes: 16 << 10,
	}, func(ctx context.Context, in *struct {
		Name    string `path:"name" maxLength:"64"`
		ScanID  string `path:"scan_id" maxLength:"36"`
		Body    NotePublicationCompleteRequest
		RawBody []byte
	}) (*struct{ Body NotePublicationStatus }, error) {
		if err := strictNoteBody(in.RawBody, &in.Body); err != nil {
			return nil, err
		}
		if err := notePathBinding(in.Name, in.ScanID, in.Body.Name, in.Body.ScanID); err != nil {
			return nil, err
		}
		if err := validateNoteRoute(d, in.Name, in.ScanID); err != nil {
			return nil, err
		}
		if !in.Body.InventoryComplete {
			return nil, invalidNoteRequest()
		}
		in.Body.Name, in.Body.ScanID = in.Name, in.ScanID
		var status store.NotePublicationStatus
		err := g.mutate(ctx, func() error {
			var err error
			status, err = d.Store.CompleteNotePublicationScan(ctx, store.NotePublicationCompleteRequest(in.Body))
			return err
		})
		if err != nil {
			return nil, notePublicationError(err)
		}
		return &struct{ Body NotePublicationStatus }{fromNotePublicationStatus(status)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getNotePublicationStatus", Method: http.MethodGet, Path: notePublicationBase,
		Summary: "Read publication freshness, conflicts and source counts",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name" maxLength:"64"`
	}) (*struct{ Body NotePublicationStatus }, error) {
		if err := validateNoteRoute(d, in.Name); err != nil {
			return nil, err
		}
		status, err := d.Store.NotePublicationStatus(ctx, in.Name)
		if err != nil {
			return nil, notePublicationError(err)
		}
		return &struct{ Body NotePublicationStatus }{fromNotePublicationStatus(status)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "acceptLocalPublishedNote", Method: http.MethodPost, Path: notePublicationBase + "/sources/accept-local",
		Summary: "Authorize a fresh local upload against an exact archive head", MaxBodyBytes: 16 << 10,
	}, func(ctx context.Context, in *struct {
		Name    string `path:"name" maxLength:"64"`
		Body    NotePublicationAcceptLocalRequest
		RawBody []byte
	}) (*struct{ Body NotePublicationRecoveryGrant }, error) {
		if err := strictNoteBody(in.RawBody, &in.Body); err != nil {
			return nil, err
		}
		if err := notePathBinding(in.Name, "", in.Body.Name, ""); err != nil {
			return nil, err
		}
		if err := validateNoteRoute(d, in.Name, in.Body.DatabaseEpoch, in.Body.ExpectedHeadVersionID, in.Body.OperationID); err != nil {
			return nil, err
		}
		if err := store.ValidatePushRef(in.Body.SourceRef); err != nil {
			return nil, invalidNoteRequest()
		}
		in.Body.Name = in.Name
		var grant store.NotePublicationRecoveryGrant
		err := g.mutate(ctx, func() error {
			var err error
			grant, err = d.Store.AcceptLocalPublishedNote(ctx, store.NotePublicationAcceptLocalRequest(in.Body))
			return err
		})
		if err != nil {
			return nil, notePublicationError(err)
		}
		return &struct{ Body NotePublicationRecoveryGrant }{NotePublicationRecoveryGrant(grant)}, nil
	})
	registerNotePublicationUploadOpenAPI(api)
	mux.HandleFunc("POST "+notePublicationBase+"/uploads", func(w http.ResponseWriter, r *http.Request) {
		handleNotePublicationUpload(w, r, d, g)
	})
}

func strictNoteBody[T any](raw []byte, target *T) error {
	if err := json.Unmarshal(raw, target, json.RejectUnknownMembers(true)); err != nil {
		return invalidNoteRequest()
	}
	return nil
}

func invalidNoteRequest() *Error {
	return NewError(422, "validation", "invalid note publication request")
}

func notePathBinding(name, scan, bodyName, bodyScan string) error {
	if (bodyName != "" && bodyName != name) || (bodyScan != "" && bodyScan != scan) {
		return invalidNoteRequest()
	}
	return nil
}

func validateNoteRoute(d Deps, name string, ids ...string) error {
	if err := checkPushName(d, name); err != nil {
		return err
	}
	for _, id := range ids {
		if store.ValidatePushRunID(id) != nil {
			return invalidNoteRequest()
		}
	}
	return nil
}

// notePublicationError never includes database errors or source bytes in a
// public diagnostic. The stable code is enough to choose recovery behavior.
func notePublicationError(err error) *Error {
	if apiErr, ok := errors.AsType[*Error](err); ok {
		return apiErr
	}
	for _, entry := range []struct {
		target error
		status int
		code   string
	}{
		{store.ErrPublicationScanSuperseded, 409, "publication_scan_superseded"},
		{store.ErrPublicationScanSealed, 409, "publication_scan_sealed"},
		{store.ErrPublicationSourceStale, 409, "publication_source_stale"},
		{store.ErrPublicationOperationMismatch, 409, "publication_operation_mismatch"},
		{store.ErrPublishedSourceDiverged, 409, "published_source_diverged"},
		{store.ErrNoteSourceUnavailable, 410, "note_source_unavailable"},
		{store.ErrNotFound, 404, "not_found"},
		{store.ErrInvalidNotePublication, 422, "validation"},
	} {
		if errors.Is(err, entry.target) {
			return NewError(entry.status, entry.code, entry.target.Error())
		}
	}
	return NewError(500, "internal", "note publication operation failed")
}

func notePublicationUploadError(err error) *Error {
	mapped := uploadError(err)
	if mapped.Status >= http.StatusInternalServerError {
		return notePublicationError(err)
	}
	return mapped
}

func fromPublishedNoteSource(source store.NotePublicationSource) NotePublicationSource {
	result := NotePublicationSource{Identity: PublishedNoteIdentity(source.Identity), Presence: source.Presence, Availability: source.Availability, Conflict: source.Conflict}
	if source.ArchiveHead != nil {
		result.ArchiveHead = new(NoteArchiveHead(*source.ArchiveHead))
	}
	if source.Recovery != nil {
		result.Recovery = new(NotePublicationRecoveryGrant(*source.Recovery))
	}
	return result
}

func fromNotePublicationStatus(status store.NotePublicationStatus) NotePublicationStatus {
	result := NotePublicationStatus{Name: status.Name, DatabaseEpoch: status.DatabaseEpoch, LastCompleteAt: status.LastCompleteAt,
		LastAcceptedAt: status.LastAcceptedAt, NamespaceRevision: status.NamespaceRevision, Incomplete: status.Incomplete,
		Present: status.Present, Absent: status.Absent, Unknown: status.Unknown, Conflicts: status.Conflicts, Unavailable: status.Unavailable, RecoveryPending: status.RecoveryPending}
	if status.ActiveScan != nil {
		result.ActiveScan = new(NotePublicationScan(*status.ActiveScan))
	}
	return result
}

func handleNotePublicationUpload(w http.ResponseWriter, r *http.Request, d Deps, g *gate) {
	query := r.URL.Query()
	request := store.NotePublicationUploadRequest{Name: r.PathValue("name"), ScanID: query.Get("scan_id"),
		SourceRef: query.Get("source_ref"), FileName: query.Get("name"), ParentPath: query.Get("parent_path"),
		ModifiedAt: query.Get("modified_at"), OperationID: query.Get("operation_id")}
	if err := validateNoteRoute(d, request.Name, request.ScanID, request.OperationID); err != nil {
		writeError(w, notePublicationError(err))
		return
	}
	var err error
	request.ExpectedSourceRevision, err = strconv.ParseInt(query.Get("expected_source_revision"), 10, 64)
	if err != nil || request.ExpectedSourceRevision < 0 ||
		store.ValidatePushSource(store.PushSource{Name: request.Name, Ref: request.SourceRef, ModifiedAt: request.ModifiedAt}) != nil ||
		store.ValidatePushDestination(request.ParentPath) != nil {
		writeError(w, invalidNoteRequest())
		return
	}
	if normalized, err := store.NormalizeName(request.FileName); err != nil || normalized != request.FileName {
		writeError(w, invalidNoteRequest())
		return
	}
	var identityErr *Error
	request.Hash, request.Size, identityErr = parseExpectedBlobIdentity(r)
	if identityErr != nil {
		writeError(w, identityErr)
		return
	}
	var receipt store.NotePublicationReceipt
	if !receiveUpload(w, r, d, g, uploadPayload{
		name: request.FileName, expectedSize: request.Size,
		errorMapper: notePublicationUploadError,
		prepare: func(ing *ingest.Ingester, mime string, body io.Reader) (*ingest.PreparedUpload, error) {
			return ing.PreparePushUpload(r.Context(), store.PushContent{Name: request.FileName, ParentPath: request.ParentPath, Hash: request.Hash, Size: request.Size, MIMEType: mime}, body)
		},
		commit: func(prepared *ingest.PreparedUpload) error {
			var err error
			receipt, err = prepared.CommitPublishedNote(r.Context(), request)
			if err != nil {
				return notePublicationError(err)
			}
			return nil
		},
	}) {
		return
	}
	status := http.StatusOK
	if receipt.Outcome == "added" {
		status = http.StatusCreated
	}
	writeJSON(w, status, NotePublicationReceipt{Scan: NotePublicationScan(receipt.Scan), Source: fromPublishedNoteSource(receipt.Source), Outcome: receipt.Outcome})
}

func registerNotePublicationUploadOpenAPI(api huma.API) {
	upload := api.OpenAPI().Paths["/api/v1/uploads"].Post
	schema := api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[NotePublicationReceipt](), true, "")
	response := &huma.Response{Description: "Exact accepted publication receipt", Content: map[string]*huma.MediaType{jsonMediaType: {Schema: schema}}}
	parameters := []*huma.Param{{Name: "name", In: openAPIPathLocation, Required: true, Description: "Publication identity", Schema: &huma.Schema{Type: openAPIStringType, MaxLength: new(64)}}}
	for _, parameter := range upload.Parameters {
		if parameter.Name == "name" || parameter.Name == BlobHashHeader || parameter.Name == BlobSizeHeader {
			parameters = append(parameters, parameter)
		}
	}
	for _, field := range []struct {
		name     string
		required bool
		max      int
	}{
		{"scan_id", true, 36}, {"source_ref", true, 4096}, {"parent_path", true, 4096}, {"operation_id", true, 36}, {"modified_at", false, 64},
	} {
		parameters = append(parameters, &huma.Param{Name: field.name, In: openAPIQueryLocation, Required: field.required, Schema: &huma.Schema{Type: openAPIStringType, MaxLength: new(field.max)}})
	}
	parameters = append(parameters, &huma.Param{Name: "expected_source_revision", In: openAPIQueryLocation, Required: true, Schema: &huma.Schema{Type: mailboxIntegerType, Format: noteRevisionFormat, Minimum: new(float64(0))}})
	responses := maps.Clone(upload.Responses)
	responses["200"], responses["201"] = response, response
	api.OpenAPI().AddOperation(&huma.Operation{OperationID: "uploadPublishedNote", Method: http.MethodPost, Path: notePublicationBase + "/uploads",
		Summary: "Upload verified original bytes with source and scan preconditions", Parameters: parameters, RequestBody: upload.RequestBody, Responses: responses})
}
