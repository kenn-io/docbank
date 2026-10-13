package daemonconn

import (
	"context"
	"errors"
	"net/http"
	"uuid"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func validatePhotoFaceResponse(face api.PhotoFace, versionID string) error {
	if !validUUIDv4(face.FaceID) || face.ContentVersionID != versionID || face.Revision < 1 || face.Box.Validate() != nil || face.DetectorFingerprint == "" || (face.AssertionID != "" && !validUUIDv4(face.AssertionID)) {
		return errors.New("invalid photo face response")
	}
	return nil
}

func (c *Connection) PhotoFaces(ctx context.Context, versionID string, cookies ...string) (api.PhotoFacePage, error) {
	if !validUUIDv4(versionID) {
		return api.PhotoFacePage{}, errors.New("invalid photo version ID")
	}
	pathID, err := uuid.Parse(versionID)
	if err != nil {
		return api.PhotoFacePage{}, errors.New("invalid photo version ID")
	}
	page, err := c.API().ListPhotoFaces(ctx, &apiclient.ListPhotoFacesRequestOptions{PathParams: &apiclient.ListPhotoFacesPath{ContentVersionID: pathID}}, photoCookie(cookies))
	if err != nil {
		return api.PhotoFacePage{}, err
	}
	if page.Faces == nil || page.Assertions == nil || len(page.Faces) > 1000 {
		return api.PhotoFacePage{}, errors.New("photo face response exceeds bound")
	}
	assertionIDs := make(map[string]bool, len(page.Assertions))
	people := make(map[string]bool, len(page.Assertions))
	for _, assertion := range page.Assertions {
		if validatePhotoAssertionResponse(assertion, versionID) != nil || assertion.Action != "assert" || assertionIDs[assertion.AssertionID] || people[assertion.PersonID] {
			return api.PhotoFacePage{}, errors.New("invalid photo assertion response")
		}
		assertionIDs[assertion.AssertionID] = true
		people[assertion.PersonID] = true
	}
	seen := make(map[string]bool, len(page.Faces))
	for _, face := range page.Faces {
		if err := validatePhotoFaceResponse(face, versionID); err != nil {
			return api.PhotoFacePage{}, err
		}
		if seen[face.FaceID] {
			return api.PhotoFacePage{}, errors.New("duplicate photo face response")
		}
		seen[face.FaceID] = true
	}
	return *page, nil
}

func (c *Connection) CreatePhotoFace(ctx context.Context, versionID string, request api.CreatePhotoFaceRequest, cookies ...string) (api.PhotoFaceResult, error) {
	if !validUUIDv4(versionID) || (request.Box == nil && request.PersonID == "") || (request.Box != nil && (request.Box.Validate() != nil || !validUUIDv4(request.FaceID))) || (request.Box == nil && request.FaceID != "") || (request.PersonID != "" && !validUUIDv4(request.PersonID)) {
		return api.PhotoFaceResult{}, errors.New("invalid photo face request")
	}
	pathID, err := uuid.Parse(versionID)
	if err != nil {
		return api.PhotoFaceResult{}, errors.New("invalid photo version ID")
	}
	var response *http.Response
	result, err := c.apiWithResponse(&response).CreatePhotoFace(ctx, &apiclient.CreatePhotoFaceRequestOptions{PathParams: &apiclient.CreatePhotoFacePath{ContentVersionID: pathID}, Body: &request}, photoCookie(cookies))
	if err != nil {
		return api.PhotoFaceResult{}, mutationRequestError(response, err)
	}
	invalid := func() (api.PhotoFaceResult, error) {
		return api.PhotoFaceResult{}, &responseDecodeError{err: errors.New("invalid photo face mutation response")}
	}
	if result.Assertion != nil && validatePhotoAssertionResponse(*result.Assertion, versionID) != nil {
		return invalid()
	}
	if request.Box == nil {
		if result.Face != nil || result.Assertion == nil {
			return invalid()
		}
	} else {
		if result.Face == nil || result.Face.FaceID != request.FaceID || result.Face.Box != *request.Box || (request.PersonID != "" && result.Assertion == nil) {
			return invalid()
		}
		if err := validatePhotoFaceResponse(*result.Face, versionID); err != nil {
			return invalid()
		}
		if result.Assertion == nil {
			if result.Face.AssertionID != "" {
				return invalid()
			}
		} else if result.Face.AssertionID != result.Assertion.AssertionID {
			return invalid()
		}
	}
	return *result, nil
}

func validatePhotoAssertionResponse(assertion api.PhotoPersonAssertion, versionID string) error {
	if !validUUIDv4(assertion.AssertionID) || !validUUIDv4(assertion.PersonID) || assertion.ContentVersionID != versionID || assertion.Role != "depicted" || (assertion.Action != "assert" && assertion.Action != "suppress") || assertion.Revision < 1 || assertion.RecordedAt == "" {
		return errors.New("invalid photo assertion response")
	}
	return nil
}
