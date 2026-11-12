package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type collectionProfileSelection struct {
	Name     string
	Names    []string
	Coverage store.CoverageSelection
}

func selectCollectionProfile(d Deps, name string, builtin bool) (collectionProfileSelection, error) {
	cfg := d.Cfg
	selected := collectionProfileSelection{Names: []string{}, Coverage: store.CoverageSelection{Configuration: "unconfigured"}}
	if len(cfg.ProcessingProfiles) > 64 {
		return selected, NewError(http.StatusServiceUnavailable, "processing_unavailable", "Too many processing profiles to inspect.")
	}
	for key := range cfg.ProcessingProfiles {
		selected.Names = append(selected.Names, key)
	}
	sort.Strings(selected.Names)
	if d.Processing != nil && (name != "" || builtin && len(selected.Names) == 0) {
		for _, profile := range d.Processing.Profiles() {
			if profile.Name == processing.PDFTextProfileName && (profile.Name == name || name == "") {
				selected.Name = profile.Name
				selected.Coverage = store.CoverageSelection{Configuration: "configured", ProfileFingerprint: profile.Fingerprint}
				if len(selected.Names) == 0 {
					selected.Names = []string{profile.Name}
				}
				return selected, nil
			}
		}
	}
	if name == "" {
		switch len(selected.Names) {
		case 0:
			return selected, nil
		case 1:
			name = selected.Names[0]
		default:
			selected.Coverage.Configuration = "profile_required"
			return selected, nil
		}
	}
	if _, ok := cfg.ProcessingProfiles[name]; !ok {
		return selected, NewError(http.StatusUnprocessableEntity, "invalid_profile", "Unknown processing profile.")
	}
	fingerprint, err := configuredProfileFingerprint(cfg, name)
	if err != nil {
		return selected, err
	}
	selected.Name = name
	selected.Coverage = store.CoverageSelection{Configuration: "configured", ProfileFingerprint: fingerprint}
	return selected, nil
}

func configuredProfileFingerprint(cfg config.Config, name string) (string, error) {
	resolved, err := cfg.ProcessingProfile(name)
	if err != nil {
		return "", NewError(http.StatusInternalServerError, "processing_configuration", "The selected processing profile is invalid.")
	}
	_, fingerprints, err := document.CanonicalProfile(resolved.Document)
	if err != nil {
		return "", NewError(http.StatusInternalServerError, "processing_configuration", "Cannot identify the selected processing profile.")
	}
	return fingerprints.Profile, nil
}

func collectionQualityError(err error) error {
	switch {
	case errors.Is(err, store.ErrInvalidQualityFields), errors.Is(err, store.ErrInvalidCoverageSelection):
		return NewError(http.StatusUnprocessableEntity, "invalid_quality", err.Error())
	case errors.Is(err, store.ErrQualityTooLarge):
		return NewError(http.StatusRequestEntityTooLarge, "quality_too_large", "Collection quality exceeds its bounded census limit.")
	case errors.Is(err, store.ErrQualityUnavailable), errors.Is(err, context.DeadlineExceeded):
		return NewError(http.StatusServiceUnavailable, "quality_unavailable", "Collection quality could not finish within its resource budget.")
	default:
		return FromStoreError(err)
	}
}

func registerCollectionQualityRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{OperationID: "getCollectionQuality", Method: http.MethodGet, Path: "/api/v1/collections/{id}/quality", Summary: "Inspect bounded current collection quality and text coverage"}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		Profile string `query:"profile" maxLength:"128"`
		Fields  string `query:"fields" maxLength:"256"`
	}) (*struct{ Body CollectionQuality }, error) {
		selection, err := selectCollectionProfile(d, in.Profile, false)
		if err != nil {
			return nil, err
		}
		var fields []string
		if in.Fields != "" {
			fields = strings.Split(in.Fields, ",")
		}
		value, err := d.Store.CollectionQuality(ctx, in.ID, selection.Coverage, fields)
		if err != nil {
			return nil, collectionQualityError(err)
		}
		return &struct{ Body CollectionQuality }{Body: fromStoreCollectionQuality(value, selection)}, nil
	})
}
