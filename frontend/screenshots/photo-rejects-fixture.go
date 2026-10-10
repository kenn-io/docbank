//go:build ignore

package main

import (
	"context"
	"os"

	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func main() {
	check := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	ctx := context.Background()
	s, err := store.Open((home.Layout{Root: os.Args[1]}).DBPath())
	check(err)
	defer func() { check(s.Close()) }()
	value, err := query.Parse([]byte(`{"sort":{"field":"name","direction":"asc"}}`))
	check(err)
	page, err := s.ListPhotoAssets(ctx, store.PhotoBrowseRequest{Query: value}, nil)
	check(err)
	var targets []store.PhotoAuthoredTarget
	for _, row := range page.Items[:3] {
		asset, err := s.PhotoAssetByID(ctx, row.AssetID)
		check(err)
		targets = append(targets, store.PhotoAuthoredTarget{FileID: asset.Files[0].ID, Revision: 1, Patch: store.PhotoAuthoredPatch{Flag: new("reject")}})
	}
	_, err = s.EditPhotoAuthored(ctx, targets)
	check(err)
	member, err := s.PhotoAssetByID(ctx, page.Items[3].AssetID)
	check(err)
	_, err = s.DetachPhotoFile(ctx, member.ID, member.Revision, member.Files[0].ID, store.PhotoDetachOptions{})
	check(err)
	asset, err := s.PhotoAssetByID(ctx, page.Items[2].AssetID)
	check(err)
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, member.Files[0].NodeID, store.PhotoRoleImage, nil)
	check(err)
}
