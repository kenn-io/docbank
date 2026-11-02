package gcs

import (
	"context"
	"errors"
	"strings"

	"cloud.google.com/go/storage"
	"go.kenn.io/kit/objectstore"
	"google.golang.org/api/iterator"
)

// ListPage preserves every entry under prefix, including foreign names. Its
// opaque token is only for continuation, never permission to remove objects.
func (s *Store) ListPage(ctx context.Context, prefix, token string, size int) (objectstore.Page, error) {
	if err := objectstore.ValidatePrefix(prefix); err != nil {
		return objectstore.Page{}, err
	}
	if size == 0 {
		size = 1000
	}
	if size < 1 || size > 1000 {
		return objectstore.Page{}, errors.New("gcs: page size must be between 1 and 1000")
	}
	query := &storage.Query{Prefix: s.prefix + prefix}
	if err := query.SetAttrSelection([]string{"Name", "Size", "Generation", "Updated"}); err != nil {
		return objectstore.Page{}, err
	}
	var items []*storage.ObjectAttrs
	next, err := iterator.NewPager(s.bucket.Objects(ctx, query), size, token).NextPage(&items)
	if err != nil {
		return objectstore.Page{}, classify("list", err)
	}
	page := objectstore.Page{NextToken: next}
	for _, item := range items {
		if !strings.HasPrefix(item.Name, s.prefix+prefix) {
			return objectstore.Page{}, errors.New("gcs: inventory entry outside prefix")
		}
		m := s.metadata(item)
		m.Checksums = nil
		page.Objects = append(page.Objects, m)
	}
	return page, nil
}
