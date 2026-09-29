package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

func registerPeopleRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "createPerson", Method: http.MethodPost, Path: "/api/v1/people",
		Summary: "Create one canonical person", DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct{ Body CreatePersonRequest }) (*personOutput, error) {
		var out *personOutput
		err := g.mutate(func() error {
			person, err := d.Store.CreatePerson(ctx, in.Body.DisplayName, "operator")
			if err != nil {
				return FromStoreError(err)
			}
			out = &personOutput{ETag: revisionETag(person.Revision), Body: fromStorePerson(person, "")}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "getPerson", Method: http.MethodGet, Path: "/api/v1/people/by-id/{person_id}",
		Summary: "Inspect one canonical person",
	}, func(ctx context.Context, in *struct {
		PersonID string `path:"person_id" format:"uuid"`
	}) (*personDetailOutput, error) {
		person, err := d.Store.PersonDetail(ctx, in.PersonID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := fromStorePersonDetail(person)
		return &personDetailOutput{ETag: revisionETag(out.Revision), Body: out}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listPersonCustodians", Method: http.MethodGet, Path: "/api/v1/people/by-id/{person_id}/custodians",
		Summary: "List active custodian assignments for one person",
	}, func(ctx context.Context, in *struct {
		PersonID string `path:"person_id" format:"uuid"`
		Limit    int    `query:"limit" default:"100" minimum:"1" maximum:"250"`
		Cursor   string `query:"cursor"`
	}) (*personCustodianPageOutput, error) {
		limit := in.Limit
		if limit == 0 {
			limit = 100
		}
		offset, err := decodePersonCustodianCursor(in.Cursor, in.PersonID)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", "invalid person custodian cursor")
		}
		assignments, total, err := d.Store.CustodiansForPerson(ctx, in.PersonID, limit, offset)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := PersonCustodianPage{Items: make([]PersonCustodianAssignment, len(assignments)), Total: total}
		for index, assignment := range assignments {
			out.Items[index] = fromStorePersonCustodian(assignment)
		}
		if offset+len(assignments) < int(total) {
			out.NextCursor = encodePersonCustodianCursor(in.PersonID, offset+len(assignments))
		}
		return &personCustodianPageOutput{Body: out}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "renamePerson", Method: http.MethodPatch, Path: "/api/v1/people/by-id/{person_id}",
		Summary: "Rename one canonical person",
	}, func(ctx context.Context, in *struct {
		PersonID string `path:"person_id" format:"uuid"`
		IfMatch  string `header:"If-Match"`
		Body     RenamePersonRequest
	}) (*personOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *personOutput
		err = g.mutate(func() error {
			person, callErr := d.Store.UpdatePerson(ctx, in.PersonID, revision, in.Body.DisplayName)
			if callErr != nil {
				return FromStoreError(callErr)
			}
			out = &personOutput{ETag: revisionETag(person.Revision), Body: fromStorePerson(person, "")}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "retirePerson", Method: http.MethodPost, Path: "/api/v1/people/by-id/{person_id}/retire",
		Summary: "Retire one canonical person",
	}, func(ctx context.Context, in *struct {
		PersonID string `path:"person_id" format:"uuid"`
		IfMatch  string `header:"If-Match"`
	}) (*personOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *personOutput
		err = g.mutate(func() error {
			person, callErr := d.Store.RetirePerson(ctx, in.PersonID, revision)
			if callErr != nil {
				return FromStoreError(callErr)
			}
			out = &personOutput{ETag: revisionETag(person.Revision), Body: fromStorePerson(person, "")}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "mergePerson", Method: http.MethodPost, Path: "/api/v1/people/by-id/{person_id}/merge",
		Summary: "Merge one person into another",
	}, func(ctx context.Context, in *struct {
		PersonID string `path:"person_id" format:"uuid"`
		IfMatch  string `header:"If-Match"`
		Body     MergePersonRequest
	}) (*personMergeOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *personMergeOutput
		err = g.mutate(func() error {
			receipt, callErr := d.Store.MergePersons(ctx, in.PersonID, in.Body.AbsorbedPersonID, in.Body.OperationID, revision, in.Body.AbsorbedRevision)
			if callErr != nil {
				return FromStoreError(callErr)
			}
			out = &personMergeOutput{ETag: revisionETag(receipt.SurvivorRevisionAfter), Body: fromStorePersonMergeReceipt(receipt)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "splitPerson", Method: http.MethodPost, Path: "/api/v1/people/by-id/{person_id}/split",
		Summary: "Split selected members into a new person",
	}, func(ctx context.Context, in *struct {
		PersonID string `path:"person_id" format:"uuid"`
		IfMatch  string `header:"If-Match"`
		Body     SplitPersonRequest
	}) (*personSplitOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *personSplitOutput
		err = g.mutate(func() error {
			external := make([]store.PersonExternalUID, len(in.Body.ExternalIdentities))
			for index, identity := range in.Body.ExternalIdentities {
				external[index] = store.PersonExternalUID{System: identity.System, ArchiveID: identity.ArchiveID, UID: identity.UID}
			}
			receipt, callErr := d.Store.SplitPerson(ctx, store.PersonSplitRequest{PersonID: in.PersonID, OperationID: in.Body.OperationID,
				DisplayName: in.Body.DisplayName, Revision: revision, IdentityIDs: in.Body.IdentityIDs, AssignmentIDs: in.Body.AssignmentIDs, External: external})
			if callErr != nil {
				return FromStoreError(callErr)
			}
			body := fromStorePersonSplitReceipt(receipt)
			body.SourceRevisionAfter = revision + 1
			out = &personSplitOutput{ETag: revisionETag(body.SourceRevisionAfter), Body: body}
			return nil
		})
		return out, err
	})
}

func encodePersonCustodianCursor(personID string, offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(personID + "\x00" + strconv.Itoa(offset)))
}

func decodePersonCustodianCursor(cursor, personID string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	parts := strings.Split(string(raw), "\x00")
	if err != nil || len(parts) != 2 || parts[0] != personID {
		return 0, errors.New("invalid cursor")
	}
	offset, err := strconv.Atoi(parts[1])
	if err != nil || offset < 0 {
		return 0, errors.New("invalid cursor")
	}
	return offset, nil
}
