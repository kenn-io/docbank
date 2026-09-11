package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

type PageFrameView struct {
	Frame  document.PageFrameV1 `json:"frame"`
	SHA256 string               `json:"sha256"`
}
type PageRecipeView struct {
	Recipe document.PageRecipeV1 `json:"recipe"`
	SHA256 string                `json:"sha256"`
}
type PageInventory struct {
	Source    document.PageSource    `json:"source"`
	PageCount int                    `json:"page_count"`
	Frames    []PageFrameView        `json:"frames"`
	Recipes   []PageRecipeView       `json:"recipes"`
	Images    []document.PageImageV1 `json:"images"`
}
type PageImageView struct {
	Frame  PageFrameView        `json:"frame"`
	Recipe PageRecipeView       `json:"recipe"`
	Image  document.PageImageV1 `json:"image"`
}

// RecordAbandonedPageBlob makes a durable but unpublished output visible to
// ordinary GC. It creates no page root and cannot enter backup authority.
func (s *Store) RecordAbandonedPageBlob(ctx context.Context, hash string, size int64, physical BlobPhysical) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error { return s.EnsureBlobTx(tx, hash, size, physical) })
}

func putPageDocument(ctx context.Context, tx *sql.Tx, d document.PageDocumentV1) error {
	encoded, hash, err := document.MarshalPageDocumentV1(d)
	if err != nil {
		return err
	}
	var existing []byte
	err = tx.QueryRowContext(ctx, `SELECT canonical_json FROM page_documents WHERE version_id=?`, d.Source.VersionID).Scan(&existing)
	if err == nil {
		if !bytes.Equal(existing, encoded) {
			return ErrPageConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO page_documents(version_id,canonical_json,checksum) VALUES(?,?,?)`, d.Source.VersionID, encoded, hash)
	if err != nil {
		return err
	}
	for _, frame := range d.Frames {
		b, h, err := document.MarshalPageFrameV1(frame)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO page_frames(version_id,page,canonical_json,checksum) VALUES(?,?,?,?)`, d.Source.VersionID, frame.Page, b, h); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) PublishPageFrames(ctx context.Context, claim PageJobClaim, frames []document.PageFrameV1) error {
	d := document.PageDocumentV1{Contract: document.PageFrameContractV1, Source: claim.Job.Request.Source, PageCount: len(frames), Frames: frames}
	if err := document.ValidatePageDocumentV1(d); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := pageClaimTx(ctx, tx, claim)
		if err != nil {
			return err
		}
		for _, page := range job.Request.Pages {
			if page > d.PageCount {
				return ErrPageLimit
			}
		}
		return putPageDocument(ctx, tx, d)
	})
}

func loadPageDocument(ctx context.Context, q metadataQuerier, versionID string) (document.PageDocumentV1, error) {
	var encoded []byte
	var hash string
	err := q.QueryRowContext(ctx, `SELECT canonical_json,checksum FROM page_documents WHERE version_id=?`, versionID).Scan(&encoded, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return document.PageDocumentV1{}, ErrNotFound
	}
	if err != nil {
		return document.PageDocumentV1{}, err
	}
	d, actual, err := document.DecodePageDocumentV1(encoded)
	if err != nil {
		return d, err
	}
	if actual != hash || d.Source.VersionID != versionID {
		return d, ErrPageConflict
	}
	rows, err := q.QueryContext(ctx, `SELECT page,canonical_json,checksum FROM page_frames WHERE version_id=? ORDER BY page`, versionID)
	if err != nil {
		return d, err
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var number int
		var raw []byte
		var digest string
		if err := rows.Scan(&number, &raw, &digest); err != nil {
			return d, err
		}
		if count >= len(d.Frames) {
			return d, ErrPageConflict
		}
		frame, actual, err := document.DecodePageFrameV1(raw)
		if err != nil {
			return d, err
		}
		if number != count+1 || frame != d.Frames[count] || actual != digest {
			return d, ErrPageConflict
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return d, err
	}
	if count != d.PageCount {
		return d, ErrPageConflict
	}
	return d, nil
}

func putPageRecipe(ctx context.Context, tx *sql.Tx, r document.PageRecipeV1) (string, error) {
	encoded, hash, err := document.MarshalPageRecipeV1(r)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO page_recipes(checksum,canonical_json) VALUES(?,?) ON CONFLICT(checksum) DO NOTHING`, hash, encoded)
	if err != nil {
		return "", err
	}
	var retained []byte
	if err := tx.QueryRowContext(ctx, `SELECT canonical_json FROM page_recipes WHERE checksum=?`, hash).Scan(&retained); err != nil {
		return "", err
	}
	if !bytes.Equal(retained, encoded) {
		return "", ErrPageConflict
	}
	return hash, nil
}

func loadPageRecipe(ctx context.Context, q metadataQuerier, hash string) (document.PageRecipeV1, error) {
	var encoded []byte
	err := q.QueryRowContext(ctx, `SELECT canonical_json FROM page_recipes WHERE checksum=?`, hash).Scan(&encoded)
	if err != nil {
		return document.PageRecipeV1{}, err
	}
	r, actual, err := document.DecodePageRecipeV1(encoded)
	if err != nil {
		return r, err
	}
	if actual != hash {
		return r, ErrPageConflict
	}
	return r, nil
}

func loadPageImage(ctx context.Context, q metadataQuerier, versionID, recipeHash string, page int) (PageImageView, error) {
	var raw, frameRaw []byte
	var hash, blobHash, frameHash string
	var blobSize int64
	err := q.QueryRowContext(ctx, `SELECT i.canonical_json,i.checksum,i.blob_hash,f.canonical_json,f.checksum,b.size FROM page_images i JOIN page_frames f ON f.version_id=i.version_id AND f.page=i.page JOIN blobs b ON b.hash=i.blob_hash WHERE i.version_id=? AND i.recipe_sha256=? AND i.page=?`, versionID, recipeHash, page).Scan(&raw, &hash, &blobHash, &frameRaw, &frameHash, &blobSize)
	if errors.Is(err, sql.ErrNoRows) {
		return PageImageView{}, ErrNotFound
	}
	if err != nil {
		return PageImageView{}, err
	}
	image, actual, err := document.DecodePageImageV1(raw)
	if err != nil {
		return PageImageView{}, err
	}
	if actual != hash || image.SHA256 != blobHash || image.Size != blobSize || image.Source.VersionID != versionID || image.Page != page || image.RecipeSHA256 != recipeHash {
		return PageImageView{}, ErrPageConflict
	}
	frame, actual, err := document.DecodePageFrameV1(frameRaw)
	if err != nil {
		return PageImageView{}, err
	}
	if actual != frameHash {
		return PageImageView{}, ErrPageConflict
	}
	recipe, err := loadPageRecipe(ctx, q, recipeHash)
	if err != nil {
		return PageImageView{}, err
	}
	if err := document.ValidatePageImageBinding(image, frame, recipe); err != nil {
		return PageImageView{}, err
	}
	return PageImageView{Frame: PageFrameView{Frame: frame, SHA256: frameHash}, Recipe: PageRecipeView{Recipe: recipe, SHA256: recipeHash}, Image: image}, nil
}

func (s *Store) PublishPageImage(ctx context.Context, claim PageJobClaim, image document.PageImageV1, recipe document.PageRecipeV1, physical *BlobPhysical) error {
	if err := document.ValidatePageImageV1(image); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		job, err := pageClaimTx(ctx, tx, claim)
		if err != nil {
			return err
		}
		if image.Source != job.Request.Source || !slices.Contains(job.Request.Pages, image.Page) {
			return ErrPageConflict
		}
		d, err := loadPageDocument(ctx, tx, image.Source.VersionID)
		if err != nil {
			return err
		}
		if image.Page > d.PageCount {
			return ErrPageConflict
		}
		if err := document.ValidatePageImageBinding(image, d.Frames[image.Page-1], recipe); err != nil {
			return err
		}
		if err := validatePageJobRecipe(job.Request, recipe); err != nil {
			return err
		}
		existing, err := loadPageImage(ctx, tx, image.Source.VersionID, image.RecipeSHA256, image.Page)
		if err == nil {
			if existing.Image != image {
				return ErrPageConflict
			}
			return appendPageJobResult(ctx, tx, job, image)
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if physical == nil {
			return errors.New("page image requires durable physical blob authority")
		}
		if _, err := putPageRecipe(ctx, tx, recipe); err != nil {
			return err
		}
		if err := s.EnsureBlobTx(tx, image.SHA256, image.Size, *physical); err != nil {
			return err
		}
		encoded, hash, err := document.MarshalPageImageV1(image)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO page_images(version_id,page,recipe_sha256,canonical_json,checksum,blob_hash) VALUES(?,?,?,?,?,?)`, image.Source.VersionID, image.Page, image.RecipeSHA256, encoded, hash, image.SHA256); err != nil {
			return err
		}
		return appendPageJobResult(ctx, tx, job, image)
	})
}

func (s *Store) PageInventory(ctx context.Context, binding PageBinding) (PageInventory, error) {
	inventory := PageInventory{Source: binding.Source, Frames: []PageFrameView{}, Recipes: []PageRecipeView{}, Images: []document.PageImageV1{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return inventory, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := pageSourceTx(ctx, tx, binding); err != nil {
		return inventory, err
	}
	d, err := loadPageDocument(ctx, tx, binding.Source.VersionID)
	if errors.Is(err, ErrNotFound) {
		return inventory, tx.Commit()
	}
	if err != nil {
		return inventory, err
	}
	if d.Source != binding.Source {
		return inventory, ErrPageConflict
	}
	inventory.PageCount = d.PageCount
	for _, frame := range d.Frames {
		_, hash, err := document.MarshalPageFrameV1(frame)
		if err != nil {
			return inventory, err
		}
		inventory.Frames = append(inventory.Frames, PageFrameView{Frame: frame, SHA256: hash})
	}
	rows, err := tx.QueryContext(ctx, `SELECT recipe_sha256,page FROM page_images WHERE version_id=? ORDER BY recipe_sha256,page`, binding.Source.VersionID)
	if err != nil {
		return inventory, err
	}
	defer func() { _ = rows.Close() }()
	type key struct {
		hash string
		page int
	}
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.hash, &k.page); err != nil {
			_ = rows.Close()
			return inventory, err
		}
		keys = append(keys, k)
		if len(keys) > 16000 {
			_ = rows.Close()
			return inventory, ErrPageLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return inventory, err
	}
	lastRecipe := ""
	for _, k := range keys {
		view, err := loadPageImage(ctx, tx, binding.Source.VersionID, k.hash, k.page)
		if err != nil {
			return inventory, err
		}
		inventory.Images = append(inventory.Images, view.Image)
		if lastRecipe != k.hash {
			inventory.Recipes = append(inventory.Recipes, view.Recipe)
			lastRecipe = k.hash
		}
	}
	return inventory, tx.Commit()
}

func (s *Store) PageImage(ctx context.Context, binding PageBinding, recipeHash string, page int) (PageImageView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PageImageView{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := pageSourceTx(ctx, tx, binding); err != nil {
		return PageImageView{}, err
	}
	view, err := loadPageImage(ctx, tx, binding.Source.VersionID, recipeHash, page)
	if err != nil {
		return view, err
	}
	if view.Image.Source != binding.Source {
		return view, ErrPageConflict
	}
	return view, tx.Commit()
}

func validatePageJobClosure(ctx context.Context, q metadataQuerier, job PageRenderJob) error {
	for _, result := range job.Results {
		view, err := loadPageImage(ctx, q, result.Source.VersionID, result.RecipeSHA256, result.Page)
		if err != nil {
			return err
		}
		if view.Image != result {
			return ErrPageConflict
		}
		if err := validatePageJobRecipe(job.Request, view.Recipe.Recipe); err != nil {
			return err
		}
	}
	return nil
}

func validatePageJobRecipe(request PageJobRequest, recipe document.PageRecipeV1) error {
	if runtime := recipe.RendererIdentity.Runtime; runtime != nil {
		encoded, err := canonical.Marshal(runtime)
		if err != nil || pageChecksum(encoded) != request.RuntimeFingerprint {
			return ErrPageConflict
		}
	}
	if request.DPI != 0 && recipe.DPI != request.DPI {
		return ErrPageConflict
	}
	return nil
}
