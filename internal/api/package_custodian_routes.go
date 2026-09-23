package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

const packageLimitParameter = "limit"

func registerPackageCustodianOpenAPI(api huma.API) {
	registry := api.OpenAPI().Components.Schemas
	errorResponse := &huma.Response{Description: "Error", Content: map[string]*huma.MediaType{
		"application/problem+json": {Schema: registry.Schema(reflect.TypeFor[Error](), true, "")},
	}}
	jsonResponse := func(description string, value reflect.Type) *huma.Response {
		return &huma.Response{Description: description, Content: map[string]*huma.MediaType{
			jsonMediaType: {Schema: registry.Schema(value, true, "")},
		}}
	}
	packageID := &huma.Param{Name: "package_id", In: openAPIPathLocation, Required: true,
		Schema: &huma.Schema{Type: openAPIStringType, Format: "uuid"}}
	assignmentID := &huma.Param{Name: "assignment_id", In: openAPIPathLocation, Required: true,
		Schema: &huma.Schema{Type: openAPIStringType, Format: "uuid"}}
	limit := &huma.Param{Name: packageLimitParameter, In: openAPIQueryLocation,
		Schema: &huma.Schema{Type: "integer", Default: 100, Minimum: new(float64(1)), Maximum: new(float64(250))}}
	cursor := &huma.Param{Name: "cursor", In: openAPIQueryLocation, Schema: &huma.Schema{Type: openAPIStringType}}
	operations := []*huma.Operation{
		{OperationID: "listPeople", Method: http.MethodGet, Path: "/api/v1/people", Summary: "Find active canonical people",
			Parameters: []*huma.Param{{Name: "query", In: openAPIQueryLocation, Schema: &huma.Schema{Type: openAPIStringType}}, limit, cursor},
			Responses:  map[string]*huma.Response{"200": jsonResponse("Bounded person candidates", reflect.TypeFor[PersonPage]()), "422": errorResponse}},
		{OperationID: "listPackageCustodians", Method: http.MethodGet, Path: "/api/v1/packages/by-id/{package_id}/custodians",
			Summary: "List active package custodian claims", Parameters: []*huma.Param{packageID,
				{Name: "row_id", In: openAPIQueryLocation, Description: "Return claims for this record only. Omit to return package-level defaults only.", Schema: &huma.Schema{Type: openAPIStringType}},
				{Name: "unresolved_only", In: openAPIQueryLocation, Schema: &huma.Schema{Type: "boolean", Default: false}}, limit, cursor},
			Responses: map[string]*huma.Response{"200": jsonResponse("Bounded custodian claims", reflect.TypeFor[CustodianPage]()), "404": errorResponse, "422": errorResponse}},
		{OperationID: "resolvePackageCustodian", Method: http.MethodPost, Path: "/api/v1/packages/custodians/{assignment_id}/resolve",
			Summary: "Resolve one exact custodian claim to a person", Parameters: []*huma.Param{assignmentID}, MaxBodyBytes: maxPackageRequestBytes,
			RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{jsonMediaType: {
				Schema: registry.Schema(reflect.TypeFor[CustodianResolveRequest](), true, "")}}},
			Responses: map[string]*huma.Response{"200": jsonResponse("Resolved custodian claim", reflect.TypeFor[CustodianAssignment]()), "404": errorResponse, "409": errorResponse, "422": errorResponse}},
		{OperationID: "assignPackageCustodian", Method: http.MethodPut, Path: "/api/v1/packages/by-id/{package_id}/custodian",
			Summary: "Create or replace an operator-owned package custodian", Parameters: []*huma.Param{packageID}, MaxBodyBytes: maxPackageRequestBytes,
			RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{jsonMediaType: {
				Schema: registry.Schema(reflect.TypeFor[PackageCustodianRequest](), true, "")}}},
			Responses: map[string]*huma.Response{"200": jsonResponse("Operator package custodian", reflect.TypeFor[CustodianAssignment]()), "404": errorResponse, "409": errorResponse, "422": errorResponse}},
	}
	for _, operation := range operations {
		for _, status := range []string{"401", "403", "500"} {
			operation.Responses[status] = errorResponse
		}
		api.OpenAPI().AddOperation(operation)
	}
}

func registerPackageCustodianRoutes(mux *http.ServeMux, d Deps, g *gate) {
	mux.HandleFunc("GET /api/v1/people", func(w http.ResponseWriter, r *http.Request) {
		limit, problem := boundedPackageQueryLimit(r, 100)
		if problem != nil {
			writeError(w, problem)
			return
		}
		query := r.URL.Query().Get("query")
		afterName, afterID, err := decodePeopleCursor(r.URL.Query().Get("cursor"), query)
		if err != nil {
			writeError(w, NewError(http.StatusUnprocessableEntity, "validation", "invalid people cursor"))
			return
		}
		people, err := d.Store.PeopleByDisplayName(r.Context(), query, afterName, afterID, limit+1)
		if err != nil {
			writePackageError(w, err)
			return
		}
		out := PersonPage{Items: make([]PersonSummary, min(limit, len(people)))}
		for index := range out.Items {
			person := people[index]
			out.Items[index] = PersonSummary{PersonID: person.PersonID, DisplayName: person.DisplayName,
				State: person.State, Revision: person.Revision}
		}
		if len(people) > limit {
			last := people[limit-1]
			out.NextCursor = encodePeopleCursor(last.DisplayNameFolded, last.PersonID, query)
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("GET /api/v1/packages/by-id/{package_id}/custodians", func(w http.ResponseWriter, r *http.Request) {
		limit, problem := boundedPackageQueryLimit(r, 100)
		if problem != nil {
			writeError(w, problem)
			return
		}
		packageID := r.PathValue("package_id")
		scope := store.CustodianScope{Kind: "package", PackageID: packageID, HasPackageRecordID: true}
		if r.URL.Query().Has("row_id") {
			scope.HasPackageRecordID = true
			scope.PackageRecordID = r.URL.Query().Get("row_id")
			if scope.PackageRecordID == "" {
				writeError(w, NewError(http.StatusUnprocessableEntity, "validation", "row_id cannot be empty"))
				return
			}
		}
		unresolved, err := strconv.ParseBool(defaultQuery(r, "unresolved_only", "false"))
		if err != nil {
			writeError(w, NewError(http.StatusUnprocessableEntity, "validation", "unresolved_only must be boolean"))
			return
		}
		offset, err := decodeCustodianCursor(r.URL.Query().Get("cursor"), packageID, scope.PackageRecordID, unresolved)
		if err != nil {
			writeError(w, NewError(http.StatusUnprocessableEntity, "validation", "invalid custodian cursor"))
			return
		}
		assignments, total, err := d.Store.Custodians(r.Context(), scope, unresolved, limit, offset)
		if err != nil {
			writePackageError(w, err)
			return
		}
		out := CustodianPage{Items: make([]CustodianAssignment, len(assignments)), Total: total}
		for index, assignment := range assignments {
			out.Items[index] = packageCustodianOutput(assignment)
		}
		if offset+len(assignments) < int(total) {
			out.NextCursor = encodeCustodianCursor(packageID, scope.PackageRecordID, unresolved, offset+len(assignments))
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("POST /api/v1/packages/custodians/{assignment_id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		var request CustodianResolveRequest
		if problem := readPackageJSON(w, r, &request); problem != nil {
			writeError(w, problem)
			return
		}
		var assignment store.CustodianAssignment
		err := g.mutate(func() error {
			var err error
			assignment, err = d.Store.ResolveCustodian(r.Context(), r.PathValue("assignment_id"), request.PersonID, request.IfMatchRevision)
			return err
		})
		if err != nil {
			writePackageError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, packageCustodianOutput(assignment))
	})

	mux.HandleFunc("PUT /api/v1/packages/by-id/{package_id}/custodian", func(w http.ResponseWriter, r *http.Request) {
		var request PackageCustodianRequest
		if problem := readPackageJSON(w, r, &request); problem != nil {
			writeError(w, problem)
			return
		}
		scope := store.CustodianScope{Kind: "package", PackageID: r.PathValue("package_id")}
		if request.RowID != nil {
			if *request.RowID == "" {
				writeError(w, NewError(http.StatusUnprocessableEntity, "validation", "row_id cannot be empty"))
				return
			}
			scope.PackageRecordID, scope.HasPackageRecordID = *request.RowID, true
		}
		var assignment store.CustodianAssignment
		err := g.mutate(func() error {
			var err error
			assignment, err = d.Store.SetOperatorPackageCustodian(r.Context(), store.CustodianRequest{
				Scope: scope, PersonID: request.PersonID, RawLabel: request.RawLabel, Rank: "primary",
				Basis: "operator_assigned", SourceRef: "operator", IfMatchRevision: request.IfMatchRevision,
			})
			return err
		})
		if err != nil {
			writePackageError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, packageCustodianOutput(assignment))
	})
}

func packageCustodianOutput(value store.CustodianAssignment) CustodianAssignment {
	out := CustodianAssignment{AssignmentID: value.AssignmentID, ScopeKind: value.ScopeKind,
		RawLabel: value.RawLabel, Rank: value.Rank, Basis: value.Basis, SourceRef: value.SourceRef,
		Revision: value.Revision, RecordedAt: value.RecordedAt}
	if value.PackageID != nil {
		out.PackageID = *value.PackageID
	}
	if value.PackageRecordID != nil {
		out.PackageRecordID = *value.PackageRecordID
	}
	if value.PersonID != nil {
		out.PersonID = *value.PersonID
	}
	return out
}

func boundedPackageQueryLimit(r *http.Request, fallback int) (int, *Error) {
	limit := fallback
	if raw := r.URL.Query().Get(packageLimitParameter); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 250 {
			return 0, NewError(http.StatusUnprocessableEntity, "validation", "limit must be between 1 and 250")
		}
		limit = parsed
	}
	return limit, nil
}

func defaultQuery(r *http.Request, key, fallback string) string {
	if value := r.URL.Query().Get(key); value != "" {
		return value
	}
	return fallback
}

func encodePeopleCursor(name, id, query string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(name + "\x00" + id + "\x00" + query))
}

func decodePeopleCursor(cursor, query string) (string, string, error) {
	if cursor == "" {
		return "", "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	parts := strings.SplitN(string(raw), "\x00", 3)
	if err != nil || len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] != query {
		return "", "", errors.New("invalid cursor")
	}
	return parts[0], parts[1], nil
}

func encodeCustodianCursor(packageID, rowID string, unresolvedOnly bool, offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(packageID + "\x00" + rowID + "\x00" + strconv.Itoa(offset) + "\x00" + strconv.FormatBool(unresolvedOnly)))
}

func decodeCustodianCursor(cursor, packageID, rowID string, unresolvedOnly bool) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	parts := strings.Split(string(raw), "\x00")
	if err != nil || len(parts) != 4 || parts[0] != packageID || parts[1] != rowID || parts[3] != strconv.FormatBool(unresolvedOnly) {
		return 0, errors.New("invalid cursor")
	}
	offset, err := strconv.Atoi(parts[2])
	if err != nil || offset < 0 {
		return 0, errors.New("invalid cursor")
	}
	return offset, nil
}
