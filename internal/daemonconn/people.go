package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"uuid"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func validatePersonETag(etag string, revision int64) error {
	if etag == "" {
		return errors.New("person response is missing ETag")
	}
	want := revisionIfMatch(revision)
	if etag != want {
		return fmt.Errorf("person response ETag %q, expected %q", etag, want)
	}
	return nil
}

func validatePersonResponse(person api.Person, etag, requestedID string) error {
	if !validUUIDv4(person.PersonID) || person.Revision < 1 {
		return errors.New("person response has invalid identity or revision")
	}
	if requestedID != "" && person.PersonID != requestedID && person.ReachedThroughPersonID != requestedID {
		return fmt.Errorf("person response ID %s does not address request %s", person.PersonID, requestedID)
	}
	return validatePersonETag(etag, person.Revision)
}

func validatePersonDetailResponse(detail api.PersonDetail, etag, requestedID string) error {
	if err := validatePersonResponse(detail.Person, etag, requestedID); err != nil {
		return err
	}
	if len(detail.Identities) > document.MaxPersonIdentitiesPerPerson || len(detail.ExternalIdentities) > document.MaxPersonExternalIdentities {
		return errors.New("person response exceeds identity bound")
	}
	for _, identity := range detail.Identities {
		if !validUUIDv4(identity.IdentityID) {
			return errors.New("person response has invalid identity ID")
		}
	}
	return nil
}

func parsePersonID(value string) (uuid.UUID, error) {
	if !validUUIDv4(value) {
		return uuid.UUID{}, errors.New("person ID must be a canonical UUIDv4")
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("parse person ID: %w", err)
	}
	return parsed, nil
}

func (c *Connection) People(ctx context.Context, query, cursor string, limit int) (api.PersonPage, error) {
	if limit < 1 || limit > 250 {
		return api.PersonPage{}, errors.New("person limit must be between 1 and 250")
	}
	options := &apiclient.ListPeopleRequestOptions{Query: &apiclient.ListPeopleQuery{Query: &query, Limit: &limit, Cursor: &cursor}}
	page, err := c.API().ListPeople(ctx, options)
	if err != nil {
		return api.PersonPage{}, err
	}
	if page == nil {
		return api.PersonPage{}, &responseDecodeError{err: errors.New("person page response is incomplete")}
	}
	if len(page.Items) > limit {
		return api.PersonPage{}, &responseDecodeError{err: errors.New("person page exceeds requested bound")}
	}
	if page.Items == nil {
		page.Items = []api.PersonSummary{}
	}
	return *page, nil
}

func (c *Connection) Person(ctx context.Context, id string) (api.PersonDetail, error) {
	pathID, err := parsePersonID(id)
	if err != nil {
		return api.PersonDetail{}, err
	}
	var response *http.Response
	detail, err := c.apiWithResponse(&response).GetPerson(ctx, &apiclient.GetPersonRequestOptions{PathParams: &apiclient.GetPersonPath{PersonID: pathID}})
	if err != nil {
		return api.PersonDetail{}, err
	}
	if detail == nil || response == nil {
		return api.PersonDetail{}, &responseDecodeError{err: errors.New("person response is incomplete")}
	}
	if err := validatePersonDetailResponse(*detail, response.Header.Get("ETag"), id); err != nil {
		return api.PersonDetail{}, &responseDecodeError{err: err}
	}
	return *detail, nil
}

func (c *Connection) PersonCustodians(ctx context.Context, id, cursor string, limit int) (api.PersonCustodianPage, error) {
	pathID, err := parsePersonID(id)
	if err != nil {
		return api.PersonCustodianPage{}, err
	}
	if limit < 1 || limit > 250 {
		return api.PersonCustodianPage{}, errors.New("person custodian limit must be between 1 and 250")
	}
	page, err := c.API().ListPersonCustodians(ctx, &apiclient.ListPersonCustodiansRequestOptions{
		PathParams: &apiclient.ListPersonCustodiansPath{PersonID: pathID},
		Query:      &apiclient.ListPersonCustodiansQuery{Limit: new(int64(limit)), Cursor: &cursor},
	})
	if err != nil {
		return api.PersonCustodianPage{}, err
	}
	if page == nil {
		return api.PersonCustodianPage{}, &responseDecodeError{err: errors.New("person custodian response is incomplete")}
	}
	if len(page.Items) > limit {
		return api.PersonCustodianPage{}, &responseDecodeError{err: errors.New("person custodian page exceeds requested bound")}
	}
	if page.Items == nil {
		page.Items = []api.PersonCustodianAssignment{}
	}
	return *page, nil
}

func (c *Connection) CreatePerson(ctx context.Context, name string) (api.Person, error) {
	var response *http.Response
	person, err := c.apiWithResponse(&response).CreatePerson(ctx, &apiclient.CreatePersonRequestOptions{Body: &apiclient.CreatePersonBody{DisplayName: name}})
	return personMutationResponse(response, person, err, "")
}

func (c *Connection) RenamePerson(ctx context.Context, id string, revision int64, name string) (api.Person, error) {
	pathID, err := parsePersonID(id)
	if err != nil || revision < 1 {
		return api.Person{}, errors.New("invalid person identity or revision")
	}
	var response *http.Response
	person, err := c.apiWithResponse(&response).RenamePerson(ctx, &apiclient.RenamePersonRequestOptions{
		PathParams: &apiclient.RenamePersonPath{PersonID: pathID}, Header: &apiclient.RenamePersonHeaders{IfMatch: revisionIfMatch(revision)},
		Body: &apiclient.RenamePersonBody{DisplayName: name},
	})
	return personMutationResponse(response, person, err, id)
}

func (c *Connection) RetirePerson(ctx context.Context, id string, revision int64) (api.Person, error) {
	pathID, err := parsePersonID(id)
	if err != nil || revision < 1 {
		return api.Person{}, errors.New("invalid person identity or revision")
	}
	var response *http.Response
	person, err := c.apiWithResponse(&response).RetirePerson(ctx, &apiclient.RetirePersonRequestOptions{
		PathParams: &apiclient.RetirePersonPath{PersonID: pathID}, Header: &apiclient.RetirePersonHeaders{IfMatch: revisionIfMatch(revision)},
	})
	return personMutationResponse(response, person, err, id)
}

func (c *Connection) MergePerson(ctx context.Context, survivorID string, survivorRevision int64, absorbedID string, absorbedRevision int64, operationID string) (api.PersonMergeReceipt, error) {
	survivor, err := parsePersonID(survivorID)
	if err != nil || !validUUIDv4(absorbedID) || !validUUIDv4(operationID) || survivorRevision < 1 || absorbedRevision < 1 {
		return api.PersonMergeReceipt{}, errors.New("invalid person merge identity or revision")
	}
	var response *http.Response
	receipt, err := c.apiWithResponse(&response).MergePerson(ctx, &apiclient.MergePersonRequestOptions{
		PathParams: &apiclient.MergePersonPath{PersonID: survivor}, Header: &apiclient.MergePersonHeaders{IfMatch: revisionIfMatch(survivorRevision)},
		Body: &apiclient.MergePersonBody{AbsorbedPersonID: absorbedID, AbsorbedRevision: absorbedRevision, OperationID: operationID},
	})
	if err != nil {
		return api.PersonMergeReceipt{}, mutationRequestError(response, err)
	}
	if receipt == nil || response == nil {
		return api.PersonMergeReceipt{}, &responseDecodeError{err: errors.New("person merge response is incomplete")}
	}
	if !validUUIDv4(receipt.MergeID) || receipt.OperationID != operationID || receipt.SurvivorPersonID != survivorID || receipt.AbsorbedPersonID != absorbedID || receipt.SurvivorRevisionAfter < 1 {
		return api.PersonMergeReceipt{}, &responseDecodeError{err: errors.New("person merge response has invalid identity")}
	}
	if err := validatePersonETag(response.Header.Get("ETag"), receipt.SurvivorRevisionAfter); err != nil {
		return api.PersonMergeReceipt{}, &responseDecodeError{err: err}
	}
	return *receipt, nil
}

func (c *Connection) SplitPerson(ctx context.Context, id string, revision int64, request api.SplitPersonRequest) (api.PersonSplitReceipt, error) {
	pathID, err := parsePersonID(id)
	if err != nil || revision < 1 || !validUUIDv4(request.OperationID) {
		return api.PersonSplitReceipt{}, errors.New("invalid person split identity or revision")
	}
	var response *http.Response
	receipt, err := c.apiWithResponse(&response).SplitPerson(ctx, &apiclient.SplitPersonRequestOptions{
		PathParams: &apiclient.SplitPersonPath{PersonID: pathID}, Header: &apiclient.SplitPersonHeaders{IfMatch: revisionIfMatch(revision)}, Body: &apiclient.SplitPersonBody{
			OperationID: request.OperationID, DisplayName: request.DisplayName, IdentityIDs: request.IdentityIDs, AssignmentIDs: request.AssignmentIDs, ExternalIdentities: request.ExternalIdentities,
		},
	})
	if err != nil {
		return api.PersonSplitReceipt{}, mutationRequestError(response, err)
	}
	if receipt == nil || response == nil {
		return api.PersonSplitReceipt{}, &responseDecodeError{err: errors.New("person split response is incomplete")}
	}
	if !validUUIDv4(receipt.OperationID) || receipt.OperationID != request.OperationID || receipt.SourcePersonID != id || !validUUIDv4(receipt.NewPersonID) || receipt.NewPersonID == id || receipt.SourceRevisionAfter < 1 {
		return api.PersonSplitReceipt{}, &responseDecodeError{err: errors.New("person split response has invalid identity")}
	}
	if err := validatePersonETag(response.Header.Get("ETag"), receipt.SourceRevisionAfter); err != nil {
		return api.PersonSplitReceipt{}, &responseDecodeError{err: err}
	}
	return *receipt, nil
}

func personMutationResponse(response *http.Response, person *api.Person, err error, requestedID string) (api.Person, error) {
	if err != nil {
		return api.Person{}, mutationRequestError(response, err)
	}
	if person == nil || response == nil {
		return api.Person{}, &responseDecodeError{err: errors.New("person response is incomplete")}
	}
	if err := validatePersonResponse(*person, response.Header.Get("ETag"), requestedID); err != nil {
		return api.Person{}, &responseDecodeError{err: err}
	}
	return *person, nil
}
