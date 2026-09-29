package daemonconn

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

func validatePhotoOwnerID(id string) error {
	if !validUUIDv4(id) {
		return errors.New("photo owner ID must be a canonical UUIDv4")
	}
	return nil
}

func validatePhotoOwnerName(name string) error {
	if name == "" {
		return errors.New("photo owner name must not be empty")
	}
	return nil
}

func validatePhotoOwner(owner api.PhotoOwner) error {
	if err := validatePhotoOwnerID(owner.ID); err != nil {
		return err
	}
	if err := validatePhotoOwnerName(owner.Name); err != nil {
		return err
	}
	if owner.Revision < 1 || owner.CreatedAt == "" || owner.UpdatedAt == "" {
		return errors.New("photo owner response has invalid revision or timestamps")
	}
	return nil
}

func (c *Connection) PhotoOwners(ctx context.Context) ([]api.PhotoOwner, error) {
	response, err := c.API().ListPhotoOwners(ctx)
	if err != nil {
		return nil, err
	}
	owners := *response
	for _, owner := range owners {
		if err := validatePhotoOwner(owner); err != nil {
			return nil, fmt.Errorf("invalid photo owner response: %w", err)
		}
	}
	return owners, nil
}

func (c *Connection) CreatePhotoOwner(ctx context.Context, name string) (api.PhotoOwner, error) {
	if err := validatePhotoOwnerName(name); err != nil {
		return api.PhotoOwner{}, err
	}
	response, err := c.API().CreatePhotoOwner(ctx, &apiclient.CreatePhotoOwnerRequestOptions{
		Body: &apiclient.CreatePhotoOwnerBody{Name: name},
	})
	if err != nil {
		return api.PhotoOwner{}, err
	}
	owner := *response
	if err := validatePhotoOwner(owner); err != nil {
		return api.PhotoOwner{}, fmt.Errorf("invalid photo owner response: %w", err)
	}
	return owner, nil
}

func (c *Connection) RenamePhotoOwner(ctx context.Context, id string, revision int64, name string) (api.PhotoOwner, error) {
	if err := validatePhotoOwnerID(id); err != nil || revision < 1 {
		if err != nil {
			return api.PhotoOwner{}, err
		}
		return api.PhotoOwner{}, errors.New("photo owner revision must be positive")
	}
	if err := validatePhotoOwnerName(name); err != nil {
		return api.PhotoOwner{}, err
	}
	response, err := c.API().RenamePhotoOwner(ctx, &apiclient.RenamePhotoOwnerRequestOptions{
		PathParams: &apiclient.RenamePhotoOwnerPath{OwnerID: id},
		Header:     &apiclient.RenamePhotoOwnerHeaders{IfMatch: photoIfMatch(revision)},
		Body:       &apiclient.RenamePhotoOwnerBody{Name: name},
	})
	if err != nil {
		return api.PhotoOwner{}, err
	}
	owner := *response
	if err := validatePhotoOwner(owner); err != nil || owner.ID != id {
		if err == nil {
			err = errors.New("photo owner response does not match request")
		}
		return api.PhotoOwner{}, fmt.Errorf("invalid photo owner response: %w", err)
	}
	return owner, nil
}

func (c *Connection) RemovePhotoOwner(ctx context.Context, id string, revision int64) error {
	if err := validatePhotoOwnerID(id); err != nil {
		return err
	}
	if revision < 1 {
		return errors.New("photo owner revision must be positive")
	}
	_, err := c.API().RemovePhotoOwner(ctx, &apiclient.RemovePhotoOwnerRequestOptions{
		PathParams: &apiclient.RemovePhotoOwnerPath{OwnerID: id},
		Header:     &apiclient.RemovePhotoOwnerHeaders{IfMatch: photoIfMatch(revision)},
	})
	return err
}
