package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPeopleRoutesWorkflow(t *testing.T) {
	ts, fixture := newTestServer(t, nil)
	coverageResponse, coverageRaw := get(t, ts, "/api/v1/people/coverage", nil)
	require.Equal(t, http.StatusOK, coverageResponse.StatusCode, coverageRaw)
	var beforeCoverage api.PeopleCoverage
	require.NoError(t, json.Unmarshal([]byte(coverageRaw), &beforeCoverage))

	createdResponse, createdBody := do(t, ts, http.MethodPost, "/api/v1/people", nil, map[string]string{"display_name": "Synthetic Ada"})
	require.Equal(t, http.StatusCreated, createdResponse.StatusCode, createdBody)
	var created api.Person
	require.NoError(t, json.Unmarshal([]byte(createdBody), &created))
	require.Equal(t, strconv.Quote("1"), createdResponse.Header.Get("ETag"))

	path := "/api/v1/people/by-id/" + created.PersonID
	missing, missingBody := do(t, ts, http.MethodPatch, path, nil, map[string]string{"display_name": "Renamed Ada"})
	require.Equal(t, http.StatusPreconditionRequired, missing.StatusCode, missingBody)
	require.Equal(t, "precondition_required", decodeProblem(t, missingBody).Code)
	malformed, malformedBody := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": "not-an-etag"}, map[string]string{"display_name": "Renamed Ada"})
	require.Equal(t, http.StatusBadRequest, malformed.StatusCode, malformedBody)
	require.Equal(t, "validation", decodeProblem(t, malformedBody).Code)
	stale, staleBody := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": strconv.Quote("99")}, map[string]string{"display_name": "Renamed Ada"})
	require.Equal(t, http.StatusPreconditionFailed, stale.StatusCode, staleBody)
	require.Equal(t, "stale_revision", decodeProblem(t, staleBody).Code)

	renamedResponse, renamedBody := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": strconv.Quote("1")}, map[string]string{"display_name": "Renamed Ada"})
	require.Equal(t, http.StatusOK, renamedResponse.StatusCode, renamedBody)
	var renamed api.Person
	require.NoError(t, json.Unmarshal([]byte(renamedBody), &renamed))
	require.Equal(t, int64(2), renamed.Revision)

	listedResponse, listedBody := get(t, ts, "/api/v1/people?query=Renamed", nil)
	require.Equal(t, http.StatusOK, listedResponse.StatusCode, listedBody)
	var page api.PersonPage
	require.NoError(t, json.Unmarshal([]byte(listedBody), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, created.PersonID, page.Items[0].PersonID)
	identity, err := fixture.AddPersonIdentity(t.Context(), created.PersonID, renamed.Revision, store.PersonIdentity{
		Kind: "name_alias", ValueDisplay: "Renamed Ada", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "synthetic-workflow", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	node := createFileWithContent(t, ts, fixture, "/people-workflow.txt", "synthetic person record")
	assignment, err := fixture.SetCustodian(t.Context(), store.CustodianRequest{
		Scope:    store.CustodianScope{Kind: "document", NodeID: node.ID, ContentVersionID: node.CurrentVersionID},
		PersonID: created.PersonID, RawLabel: "Renamed Ada", Rank: "primary", Basis: "operator_assigned",
		SourceRef: "api-workflow", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	absorbedResponse, absorbedBody := do(t, ts, http.MethodPost, "/api/v1/people", nil, map[string]string{"display_name": "Synthetic absorbed"})
	require.Equal(t, http.StatusCreated, absorbedResponse.StatusCode, absorbedBody)
	var absorbed api.Person
	require.NoError(t, json.Unmarshal([]byte(absorbedBody), &absorbed))
	mergeBody := map[string]any{"absorbed_person_id": absorbed.PersonID, "absorbed_revision": absorbed.Revision, "operation_id": "00000000-0000-4000-8000-000000000003"}
	staleAbsorbedBody := map[string]any{"absorbed_person_id": absorbed.PersonID, "absorbed_revision": absorbed.Revision + 1, "operation_id": "00000000-0000-4000-8000-000000000007"}
	staleAbsorbedResponse, staleAbsorbedRaw := do(t, ts, http.MethodPost, path+"/merge", map[string]string{"If-Match": strconv.Quote("3")}, staleAbsorbedBody)
	require.Equal(t, http.StatusPreconditionFailed, staleAbsorbedResponse.StatusCode, staleAbsorbedRaw)
	require.Equal(t, "stale_revision", decodeProblem(t, staleAbsorbedRaw).Code)
	mergeResponse, mergeRaw := do(t, ts, http.MethodPost, path+"/merge", map[string]string{"If-Match": strconv.Quote("3")}, mergeBody)
	require.Equal(t, http.StatusOK, mergeResponse.StatusCode, mergeRaw)
	var merge api.PersonMergeReceipt
	require.NoError(t, json.Unmarshal([]byte(mergeRaw), &merge))
	require.Equal(t, int64(4), merge.SurvivorRevisionAfter)
	require.Equal(t, strconv.Quote(strconv.FormatInt(merge.SurvivorRevisionAfter, 10)), mergeResponse.Header.Get("ETag"))
	conflictMergeBody := map[string]any{"absorbed_person_id": absorbed.PersonID, "absorbed_revision": absorbed.Revision + 1, "operation_id": "00000000-0000-4000-8000-000000000003"}
	conflictMergeResponse, conflictMergeRaw := do(t, ts, http.MethodPost, path+"/merge", map[string]string{"If-Match": strconv.Quote("3")}, conflictMergeBody)
	require.Equal(t, http.StatusConflict, conflictMergeResponse.StatusCode, conflictMergeRaw)
	require.Equal(t, "person_merge_conflict", decodeProblem(t, conflictMergeRaw).Code)
	replayResponse, replayRaw := do(t, ts, http.MethodPost, path+"/merge", map[string]string{"If-Match": strconv.Quote("3")}, mergeBody)
	require.Equal(t, http.StatusOK, replayResponse.StatusCode, replayRaw)
	require.JSONEq(t, mergeRaw, replayRaw)
	aliasResponse, aliasRaw := get(t, ts, "/api/v1/people/by-id/"+absorbed.PersonID, nil)
	require.Equal(t, http.StatusOK, aliasResponse.StatusCode, aliasRaw)
	var alias api.PersonDetail
	require.NoError(t, json.Unmarshal([]byte(aliasRaw), &alias))
	require.Equal(t, created.PersonID, alias.PersonID)
	require.Equal(t, absorbed.PersonID, alias.ReachedThroughPersonID)
	coverageResponse, coverageRaw = get(t, ts, "/api/v1/people/coverage", nil)
	require.Equal(t, http.StatusOK, coverageResponse.StatusCode, coverageRaw)
	var afterMergeCoverage api.PeopleCoverage
	require.NoError(t, json.Unmarshal([]byte(coverageRaw), &afterMergeCoverage))
	require.Greater(t, afterMergeCoverage.BindingEpoch, beforeCoverage.BindingEpoch)
	emptySplitBody := map[string]any{"operation_id": "00000000-0000-4000-8000-000000000008", "display_name": "Empty split"}
	emptySplitResponse, emptySplitRaw := do(t, ts, http.MethodPost, path+"/split", map[string]string{"If-Match": strconv.Quote("4")}, emptySplitBody)
	require.Equal(t, http.StatusUnprocessableEntity, emptySplitResponse.StatusCode, emptySplitRaw)
	require.Equal(t, "invalid_person", decodeProblem(t, emptySplitRaw).Code)
	splitBody := map[string]any{"operation_id": "00000000-0000-4000-8000-000000000004", "display_name": "Split Ada", "identity_ids": []string{identity.IdentityID}, "assignment_ids": []string{assignment.AssignmentID}}
	splitResponse, splitRaw := do(t, ts, http.MethodPost, path+"/split", map[string]string{"If-Match": strconv.Quote("4")}, splitBody)
	require.Equal(t, http.StatusOK, splitResponse.StatusCode, splitRaw)
	var split api.PersonSplitReceipt
	require.NoError(t, json.Unmarshal([]byte(splitRaw), &split))
	require.Equal(t, created.PersonID, split.SourcePersonID)
	require.Equal(t, int64(5), split.SourceRevisionAfter)
	require.Equal(t, strconv.Quote("5"), splitResponse.Header.Get("ETag"))
	renameAfterSplitResponse, renameAfterSplitRaw := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": strconv.Quote("5")}, map[string]string{"display_name": "Renamed after split"})
	require.Equal(t, http.StatusOK, renameAfterSplitResponse.StatusCode, renameAfterSplitRaw)
	splitReplayBody := map[string]any{"operation_id": splitBody["operation_id"], "display_name": splitBody["display_name"], "identity_ids": splitBody["identity_ids"], "assignment_ids": splitBody["assignment_ids"], "external_identities": []any{}}
	splitReplayResponse, splitReplayRaw := do(t, ts, http.MethodPost, path+"/split", map[string]string{"If-Match": strconv.Quote("4")}, splitReplayBody)
	require.Equal(t, http.StatusOK, splitReplayResponse.StatusCode, splitReplayRaw)
	require.JSONEq(t, splitRaw, splitReplayRaw)
	require.Equal(t, strconv.Quote("5"), splitReplayResponse.Header.Get("ETag"))
	changedSplitBody := map[string]any{"operation_id": splitBody["operation_id"], "display_name": "Changed split", "identity_ids": splitBody["identity_ids"], "assignment_ids": splitBody["assignment_ids"]}
	changedSplitResponse, changedSplitRaw := do(t, ts, http.MethodPost, path+"/split", map[string]string{"If-Match": strconv.Quote("4")}, changedSplitBody)
	require.Equal(t, http.StatusConflict, changedSplitResponse.StatusCode, changedSplitRaw)
	require.Equal(t, "person_merge_conflict", decodeProblem(t, changedSplitRaw).Code)
	custodianResponse, custodianRaw := get(t, ts, "/api/v1/people/by-id/"+split.NewPersonID+"/custodians?limit=1", nil)
	require.Equal(t, http.StatusOK, custodianResponse.StatusCode, custodianRaw)
	var custodianPage api.PersonCustodianPage
	require.NoError(t, json.Unmarshal([]byte(custodianRaw), &custodianPage))
	require.Len(t, custodianPage.Items, 1)
	require.Equal(t, assignment.AssignmentID, custodianPage.Items[0].AssignmentID)
	require.Equal(t, node.ID, custodianPage.Items[0].NodeID)
	require.Equal(t, node.CurrentVersionID, custodianPage.Items[0].ContentVersionID)
	coverageResponse, coverageRaw = get(t, ts, "/api/v1/people/coverage", nil)
	require.Equal(t, http.StatusOK, coverageResponse.StatusCode, coverageRaw)
	var afterSplitCoverage api.PeopleCoverage
	require.NoError(t, json.Unmarshal([]byte(coverageRaw), &afterSplitCoverage))
	require.Greater(t, afterSplitCoverage.BindingEpoch, afterMergeCoverage.BindingEpoch)

	retiredResponse, retiredBody := do(t, ts, http.MethodPost, path+"/retire", map[string]string{"If-Match": strconv.Quote("6")}, nil)
	require.Equal(t, http.StatusOK, retiredResponse.StatusCode, retiredBody)
	var retired api.Person
	require.NoError(t, json.Unmarshal([]byte(retiredBody), &retired))
	require.Equal(t, "retired", retired.State)
	retiredGetResponse, retiredGetRaw := get(t, ts, path, nil)
	require.Equal(t, http.StatusNotFound, retiredGetResponse.StatusCode, retiredGetRaw)
	require.Equal(t, "not_found", decodeProblem(t, retiredGetRaw).Code)
	retiredRenameResponse, retiredRenameRaw := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": strconv.Quote("6")}, map[string]string{"display_name": "Retired rename"})
	require.Equal(t, http.StatusConflict, retiredRenameResponse.StatusCode, retiredRenameRaw)
	require.Equal(t, "person_retired", decodeProblem(t, retiredRenameRaw).Code)

	listedResponse, listedBody = get(t, ts, "/api/v1/people?query=Renamed", nil)
	require.Equal(t, http.StatusOK, listedResponse.StatusCode, listedBody)
	require.NoError(t, json.Unmarshal([]byte(listedBody), &page))
	require.Empty(t, page.Items)
}

func TestPeopleRouteCustodiansPreserveAllScopeCoordinates(t *testing.T) {
	ts, fixture := newTestServer(t, nil)
	createdResponse, createdBody := do(t, ts, http.MethodPost, "/api/v1/people", nil, map[string]string{"display_name": "Synthetic coordinates"})
	require.Equal(t, http.StatusCreated, createdResponse.StatusCode, createdBody)
	var person api.Person
	require.NoError(t, json.Unmarshal([]byte(createdBody), &person))
	collection, err := fixture.BeginIngest(t.Context(), "cli", "synthetic collection")
	require.NoError(t, err)
	_, err = fixture.IngestFileExact(t.Context(), collection, fixture.RootID(), "collection.txt", testHash("synthetic collection"), int64(len("synthetic collection")), "text/plain", "collection.txt", "")
	require.NoError(t, err)
	collectionAssignment, err := fixture.SetCustodian(t.Context(), store.CustodianRequest{
		Scope: store.CustodianScope{Kind: "collection", IngestID: collection.ID()}, PersonID: person.PersonID,
		RawLabel: "Collection owner", Rank: "primary", Basis: "operator_assigned", SourceRef: "synthetic-collection", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	pkg, _, _ := seedBrowseReceivedPackage(t, fixture, false)
	packageAssignment, err := fixture.SetCustodian(t.Context(), store.CustodianRequest{
		Scope: store.CustodianScope{Kind: "package", PackageID: pkg.PackageID}, PersonID: person.PersonID,
		RawLabel: "Package owner", Rank: "primary", Basis: "operator_assigned", SourceRef: "synthetic-package", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	node := createFileWithContent(t, ts, fixture, "/coordinates.txt", "synthetic coordinates")
	documentAssignment, err := fixture.SetCustodian(t.Context(), store.CustodianRequest{
		Scope: store.CustodianScope{Kind: "document", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}, PersonID: person.PersonID,
		RawLabel: "Document owner", Rank: "primary", Basis: "operator_assigned", SourceRef: "synthetic-document", IfMatchRevision: 1,
	})
	require.NoError(t, err)

	response, body := get(t, ts, "/api/v1/people/by-id/"+person.PersonID+"/custodians?limit=10", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PersonCustodianPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.EqualValues(t, 3, page.Total)
	require.Len(t, page.Items, 3)
	byID := make(map[string]api.PersonCustodianAssignment, len(page.Items))
	for _, item := range page.Items {
		byID[item.AssignmentID] = item
	}
	collectionOutput := byID[collectionAssignment.AssignmentID]
	require.Equal(t, collection.ID(), collectionOutput.IngestID)
	require.Empty(t, collectionOutput.PackageID)
	require.Zero(t, collectionOutput.NodeID)
	packageOutput := byID[packageAssignment.AssignmentID]
	require.Equal(t, pkg.PackageID, packageOutput.PackageID)
	require.Empty(t, packageOutput.IngestID)
	require.Zero(t, packageOutput.NodeID)
	documentOutput := byID[documentAssignment.AssignmentID]
	require.Equal(t, node.ID, documentOutput.NodeID)
	require.Equal(t, node.CurrentVersionID, documentOutput.ContentVersionID)
	require.Empty(t, documentOutput.IngestID)
	require.Empty(t, documentOutput.PackageID)
}

func TestPeopleRoutesUseInheritedBodyLimit(t *testing.T) {
	ts, fixture := newTestServer(t, nil)
	createdResponse, createdBody := do(t, ts, http.MethodPost, "/api/v1/people", nil, map[string]string{"display_name": "Synthetic body limit"})
	require.Equal(t, http.StatusCreated, createdResponse.StatusCode, createdBody)
	var person api.Person
	require.NoError(t, json.Unmarshal([]byte(createdBody), &person))
	assignments := make([]string, 100000)
	for index := range assignments {
		assignments[index] = fmt.Sprintf("00000000-0000-4000-8000-%012x", index+1)
	}
	response, body := do(t, ts, http.MethodPost, "/api/v1/people/by-id/"+person.PersonID+"/split", map[string]string{"If-Match": strconv.Quote("1")}, map[string]any{
		"operation_id": "00000000-0000-4000-8000-000000000001", "display_name": "Oversized split", "assignment_ids": assignments,
	})
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode, body)
	current, _, err := fixture.PersonByID(t.Context(), person.PersonID)
	require.NoError(t, err)
	require.Equal(t, int64(1), current.Revision)
}

func TestPeopleRoutesEnforceIfMatch(t *testing.T) {
	ts, fixture := newTestServer(t, nil)
	createdResponse, createdBody := do(t, ts, http.MethodPost, "/api/v1/people", nil, map[string]string{"display_name": "Synthetic survivor"})
	require.Equal(t, http.StatusCreated, createdResponse.StatusCode, createdBody)
	var survivor api.Person
	require.NoError(t, json.Unmarshal([]byte(createdBody), &survivor))
	identity, err := fixture.AddPersonIdentity(t.Context(), survivor.PersonID, survivor.Revision, store.PersonIdentity{
		Kind: "name_alias", ValueDisplay: "Synthetic survivor", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "synthetic-evidence", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	absorbedResponse, absorbedBody := do(t, ts, http.MethodPost, "/api/v1/people", nil, map[string]string{"display_name": "Synthetic absorbed"})
	require.Equal(t, http.StatusCreated, absorbedResponse.StatusCode, absorbedBody)
	var absorbed api.Person
	require.NoError(t, json.Unmarshal([]byte(absorbedBody), &absorbed))
	base := "/api/v1/people/by-id/" + survivor.PersonID
	operationID := "00000000-0000-4000-8000-000000000003"
	tests := []struct {
		name, method, path string
		body               any
	}{
		{"rename", http.MethodPatch, base, map[string]string{"display_name": "Renamed"}},
		{"retire", http.MethodPost, base + "/retire", nil},
		{"merge", http.MethodPost, base + "/merge", map[string]any{"absorbed_person_id": absorbed.PersonID, "absorbed_revision": absorbed.Revision, "operation_id": operationID}},
		{"split", http.MethodPost, base + "/split", map[string]any{"operation_id": operationID, "display_name": "Split", "identity_ids": []string{identity.IdentityID}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			missing, missingBody := do(t, ts, test.method, test.path, nil, test.body)
			require.Equal(t, http.StatusPreconditionRequired, missing.StatusCode, missingBody)
			require.Equal(t, "precondition_required", decodeProblem(t, missingBody).Code)
			malformed, malformedBody := do(t, ts, test.method, test.path, map[string]string{"If-Match": "bad"}, test.body)
			require.Equal(t, http.StatusBadRequest, malformed.StatusCode, malformedBody)
			stale, staleBody := do(t, ts, test.method, test.path, map[string]string{"If-Match": strconv.Quote("99")}, test.body)
			require.Equal(t, http.StatusPreconditionFailed, stale.StatusCode, staleBody)
			require.Equal(t, "stale_revision", decodeProblem(t, staleBody).Code)
		})
	}
}

func TestPeopleRoutesRefuseAuditedVaultAndKeepReads(t *testing.T) {
	ts, fixture := newTestServer(t, nil)
	createdResponse, createdBody := do(t, ts, http.MethodPost, "/api/v1/people", nil, map[string]string{"display_name": "Synthetic audited"})
	require.Equal(t, http.StatusCreated, createdResponse.StatusCode, createdBody)
	var person api.Person
	require.NoError(t, json.Unmarshal([]byte(createdBody), &person))
	identity, err := fixture.AddPersonIdentity(t.Context(), person.PersonID, person.Revision, store.PersonIdentity{
		Kind: "name_alias", ValueDisplay: "Synthetic audited", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "synthetic-audit", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	person.Revision++
	absorbedResponse, absorbedBody := do(t, ts, http.MethodPost, "/api/v1/people", nil, map[string]string{"display_name": "Synthetic absorbed"})
	require.Equal(t, http.StatusCreated, absorbedResponse.StatusCode, absorbedBody)
	var absorbed api.Person
	require.NoError(t, json.Unmarshal([]byte(absorbedBody), &absorbed))
	c := daemonconn.New(ts.URL, testAPIKey)
	preview, err := c.PreviewAudit(t.Context(), daemonconn.AuditPreviewOptions{NodeID: fixture.RootID()})
	require.NoError(t, err)
	_, err = c.EnableAudit(t.Context(), preview.PreviewToken, true)
	require.NoError(t, err)

	base := "/api/v1/people/by-id/" + person.PersonID
	mutationTests := []struct {
		name, method, path string
		body               any
		headers            map[string]string
	}{
		{"create", http.MethodPost, "/api/v1/people", map[string]string{"display_name": "After audit"}, nil},
		{"rename", http.MethodPatch, base, map[string]string{"display_name": "After audit rename"}, map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(person.Revision, 10))}},
		{"retire", http.MethodPost, base + "/retire", nil, map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(person.Revision, 10))}},
		{"merge", http.MethodPost, base + "/merge", map[string]any{"absorbed_person_id": absorbed.PersonID, "absorbed_revision": absorbed.Revision, "operation_id": "00000000-0000-4000-8000-000000000004"}, map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(person.Revision, 10))}},
		{"split", http.MethodPost, base + "/split", map[string]any{"operation_id": "00000000-0000-4000-8000-000000000005", "display_name": "Split", "identity_ids": []string{identity.IdentityID}}, map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(person.Revision, 10))}},
	}
	for _, test := range mutationTests {
		t.Run(test.name, func(t *testing.T) {
			resp, body := do(t, ts, test.method, test.path, test.headers, test.body)
			require.Equal(t, http.StatusConflict, resp.StatusCode, body)
			require.Equal(t, "audit_mutation_unsupported", decodeProblem(t, body).Code)
		})
	}
	read, body := get(t, ts, base, nil)
	require.Equal(t, http.StatusOK, read.StatusCode, body)
	var unchanged api.PersonDetail
	require.NoError(t, json.Unmarshal([]byte(body), &unchanged))
	require.Equal(t, person.PersonID, unchanged.PersonID)
	require.Equal(t, person.Revision, unchanged.Revision)
	list, body := get(t, ts, "/api/v1/people", nil)
	require.Equal(t, http.StatusOK, list.StatusCode, body)
}

func TestPeopleListExcludesRetiredAfterRouteRetire(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	createdResponse, createdBody := do(t, ts, http.MethodPost, "/api/v1/people", nil, map[string]string{"display_name": "Synthetic list person"})
	require.Equal(t, http.StatusCreated, createdResponse.StatusCode, createdBody)
	var person api.Person
	require.NoError(t, json.Unmarshal([]byte(createdBody), &person))
	retiredResponse, retiredBody := do(t, ts, http.MethodPost, "/api/v1/people/by-id/"+person.PersonID+"/retire", map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(person.Revision, 10))}, nil)
	require.Equal(t, http.StatusOK, retiredResponse.StatusCode, retiredBody)
	listResponse, listBody := get(t, ts, "/api/v1/people?query=Synthetic+list", nil)
	require.Equal(t, http.StatusOK, listResponse.StatusCode, listBody)
	var page api.PersonPage
	require.NoError(t, json.Unmarshal([]byte(listBody), &page))
	require.Empty(t, page.Items)
}
