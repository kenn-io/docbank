package docbank

import (
	"context"
	"fmt"

	"go.kenn.io/docbank/document"
	internalprocessing "go.kenn.io/docbank/internal/processing"
)

// VisualPreviewSupportsMediaType reports whether the built-in decoder supports a media type.
func VisualPreviewSupportsMediaType(mediaType string) bool {
	return internalprocessing.VisualPreviewSupportsMediaType(mediaType)
}

func visualPreviewProcessingError(err error) error {
	if internalprocessing.IsSourceContentUnavailable(err) {
		return fmt.Errorf("producing visual preview: %w: %w", ErrContentUnavailable, err)
	}
	return err
}

func visualPreviewRecipe(size VisualPreviewSize) (document.VisualPreviewRecipeV1, string, error) {
	recipe, err := internalprocessing.VisualPreviewRecipeForSize(string(size))
	if err != nil {
		return recipe, "", err
	}
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
	return recipe, fingerprint, err
}

// VisualPreviewForSize reads the exact selected recipe, including terminal failures.
func (v *Vault) VisualPreviewForSize(ctx context.Context, versionID string, size VisualPreviewSize) (VisualPreview, error) {
	if err := v.begin(); err != nil {
		return VisualPreview{}, err
	}
	defer v.lifecycle.RUnlock()
	_, fingerprint, err := visualPreviewRecipe(size)
	if err != nil {
		return VisualPreview{}, err
	}
	view, err := v.metadata.ContentVersionVisualPreviewByRecipe(ctx, versionID, fingerprint)
	if err != nil {
		return VisualPreview{}, err
	}
	return fromStoreVisualPreview(view), nil
}

// EnsureVisualPreviewForSize produces or reuses the exact selected recipe.
func (v *Vault) EnsureVisualPreviewForSize(ctx context.Context, versionID string, size VisualPreviewSize) (VisualPreview, error) {
	if err := v.begin(); err != nil {
		return VisualPreview{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	recipe, _, err := visualPreviewRecipe(size)
	if err != nil {
		return VisualPreview{}, err
	}
	view, err := internalprocessing.EnsureVisualPreview(ctx, v.metadata, v.blobs, versionID, recipe)
	if err != nil {
		return VisualPreview{}, visualPreviewProcessingError(err)
	}
	return fromStoreVisualPreview(view), nil
}

// OpenVisualPreviewForSize streams verified bytes for the exact selected recipe.
func (v *Vault) OpenVisualPreviewForSize(ctx context.Context, versionID string, size VisualPreviewSize) (*VisualPreviewContent, error) {
	if err := v.begin(); err != nil {
		return nil, err
	}
	_, fingerprint, err := visualPreviewRecipe(size)
	if err != nil {
		v.lifecycle.RUnlock()
		return nil, err
	}
	view, err := v.metadata.ContentVersionVisualPreviewByRecipe(ctx, versionID, fingerprint)
	if err != nil {
		v.lifecycle.RUnlock()
		return nil, err
	}
	return v.openVisualPreviewView(ctx, versionID, view)
}
