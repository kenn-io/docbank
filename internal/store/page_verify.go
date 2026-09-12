package store

import (
	"context"
	"errors"
	"io"

	"go.kenn.io/docbank/document/pagerender"
)

// VerifyPageImageBytes checks complete image decoding and exact retained
// source/frame/recipe closure after metadata import and physical restoration.
func (s *Store) VerifyPageImageBytes(ctx context.Context, reader RenditionBlobReader) error {
	rows, err := s.db.QueryContext(ctx, `SELECT version_id,recipe_sha256,page FROM page_images ORDER BY version_id,recipe_sha256,page`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type key struct {
		version, recipe string
		page            int
	}
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.version, &k.recipe, &k.page); err != nil {
			_ = rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, k := range keys {
		view, err := loadPageImage(ctx, s.db, k.version, k.recipe, k.page)
		if err != nil {
			return err
		}
		stream, size, err := reader.OpenStreamContext(ctx, view.Image.SHA256)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(stream, view.Image.Size+1))
		verified := stream.Verified() && size == view.Image.Size
		closeErr := stream.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if !verified {
			return pagerender.ErrInvalidOutput
		}
		if err := pagerender.VerifyPNG(data, view.Image); err != nil {
			return err
		}
	}
	return nil
}
