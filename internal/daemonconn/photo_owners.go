package daemonconn

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func validatePhotoOwner(owner api.PhotoOwner) error {
	if !validUUIDv4(owner.ID) || owner.Name == "" || owner.EnrolledAt == "" {
		return errors.New("invalid photo owner response")
	}
	return nil
}

func validatePhotoOwnerTarget(personID string, revision int64) error {
	if !validUUIDv4(personID) {
		return errors.New("person ID must be a canonical UUIDv4")
	}
	if revision < 1 {
		return errors.New("person revision must be positive")
	}
	return nil
}

func (c *Connection) PhotoOwners(ctx context.Context) ([]api.PhotoOwner, error) {
	response, err := c.API().ListPhotoOwners(ctx)
	if err != nil {
		return nil, err
	}
	for _, owner := range *response {
		if err := validatePhotoOwner(owner); err != nil {
			return nil, err
		}
	}
	return *response, nil
}

// EnrollPhotoOwner enrolls a person, fenced on the person's revision.
func (c *Connection) EnrollPhotoOwner(ctx context.Context, personID string, revision int64) (api.PhotoOwner, error) {
	if err := validatePhotoOwnerTarget(personID, revision); err != nil {
		return api.PhotoOwner{}, err
	}
	response, err := c.API().EnrollPhotoOwner(ctx, &apiclient.EnrollPhotoOwnerRequestOptions{
		Header: &apiclient.EnrollPhotoOwnerHeaders{IfMatch: revisionIfMatch(revision)},
		Body:   &apiclient.EnrollPhotoOwnerBody{PersonID: personID},
	})
	if err != nil {
		return api.PhotoOwner{}, err
	}
	if response.ID != personID {
		return api.PhotoOwner{}, errors.New("photo owner response does not match the enrolled person")
	}
	return *response, validatePhotoOwner(*response)
}

// RemovePhotoOwner ends an enrollment, fenced on the person's revision.
func (c *Connection) RemovePhotoOwner(ctx context.Context, personID string, revision int64) error {
	if err := validatePhotoOwnerTarget(personID, revision); err != nil {
		return err
	}
	_, err := c.API().RemovePhotoOwner(ctx, &apiclient.RemovePhotoOwnerRequestOptions{
		PathParams: &apiclient.RemovePhotoOwnerPath{PersonID: personID},
		Header:     &apiclient.RemovePhotoOwnerHeaders{IfMatch: revisionIfMatch(revision)},
	})
	return err
}
