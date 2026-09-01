package document

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var visualPreviewFailureCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// MarshalVisualPreviewRecipeV1 validates and fingerprints one recipe.
func MarshalVisualPreviewRecipeV1(value VisualPreviewRecipeV1) ([]byte, string, error) {
	canonical, err := canonicalVisualPreviewRecipeV1(value)
	if err != nil {
		return nil, "", err
	}
	encoded, err := json.Marshal(canonical, json.Deterministic(true))
	if err != nil {
		return nil, "", fmt.Errorf("encoding visual preview recipe: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(digest[:]), nil
}

// DecodeVisualPreviewRecipeV1 accepts only the exact canonical v1 encoding.
func DecodeVisualPreviewRecipeV1(encoded []byte) (VisualPreviewRecipeV1, string, error) {
	var value VisualPreviewRecipeV1
	if err := json.Unmarshal(encoded, &value, json.RejectUnknownMembers(true)); err != nil {
		return VisualPreviewRecipeV1{}, "", fmt.Errorf("decoding visual preview recipe: %w", err)
	}
	canonical, fingerprint, err := MarshalVisualPreviewRecipeV1(value)
	if err != nil {
		return VisualPreviewRecipeV1{}, "", err
	}
	if !slices.Equal(encoded, canonical) {
		return VisualPreviewRecipeV1{}, "", errors.New("visual preview recipe bytes are not canonical")
	}
	return value, fingerprint, nil
}

// MarshalVisualPreviewV1 validates, canonicalizes, and hashes one result.
func MarshalVisualPreviewV1(value VisualPreviewV1) ([]byte, string, error) {
	canonical, err := canonicalVisualPreviewV1(value)
	if err != nil {
		return nil, "", err
	}
	encoded, err := json.Marshal(canonical, json.Deterministic(true))
	if err != nil {
		return nil, "", fmt.Errorf("encoding visual preview: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(digest[:]), nil
}

// DecodeVisualPreviewV1 accepts only the exact canonical v1 encoding.
func DecodeVisualPreviewV1(encoded []byte) (VisualPreviewV1, string, error) {
	var value VisualPreviewV1
	if err := json.Unmarshal(encoded, &value, json.RejectUnknownMembers(true)); err != nil {
		return VisualPreviewV1{}, "", fmt.Errorf("decoding visual preview: %w", err)
	}
	canonical, checksum, err := MarshalVisualPreviewV1(value)
	if err != nil {
		return VisualPreviewV1{}, "", err
	}
	if !slices.Equal(encoded, canonical) {
		return VisualPreviewV1{}, "", errors.New("visual preview bytes are not canonical")
	}
	return value, checksum, nil
}

func canonicalVisualPreviewRecipeV1(value VisualPreviewRecipeV1) (VisualPreviewRecipeV1, error) {
	if value.ContractVersion != VisualPreviewContractV1 {
		return VisualPreviewRecipeV1{}, fmt.Errorf(
			"visual preview recipe contract version must be %q", VisualPreviewContractV1)
	}
	if value.MaxEdgePixels < 1 || value.MaxEdgePixels > MaxVisualPreviewEdgePixels {
		return VisualPreviewRecipeV1{}, fmt.Errorf(
			"visual preview maximum edge must be between 1 and %d pixels", MaxVisualPreviewEdgePixels)
	}
	if value.OrientationPolicy != "apply" {
		return VisualPreviewRecipeV1{}, errors.New("visual preview orientation policy must be apply")
	}
	if value.ColorPolicy != "srgb" {
		return VisualPreviewRecipeV1{}, errors.New("visual preview color policy must be srgb")
	}
	if value.FramePolicy != "primary" && value.FramePolicy != "representative" {
		return VisualPreviewRecipeV1{}, errors.New(
			"visual preview frame policy must be primary or representative")
	}
	switch value.OutputMediaType {
	case "image/jpeg", "image/png", "image/webp":
	default:
		return VisualPreviewRecipeV1{}, errors.New("visual preview output media type is unsupported")
	}
	if !canonicalSHA256(value.ProcessorFingerprint) {
		return VisualPreviewRecipeV1{}, errors.New("visual preview processor fingerprint is invalid")
	}
	return value, nil
}

func canonicalVisualPreviewV1(value VisualPreviewV1) (VisualPreviewV1, error) {
	if value.ContractVersion != VisualPreviewContractV1 {
		return VisualPreviewV1{}, fmt.Errorf(
			"visual preview contract version must be %q", VisualPreviewContractV1)
	}
	recipe, err := canonicalVisualPreviewRecipeV1(value.Recipe)
	if err != nil {
		return VisualPreviewV1{}, err
	}
	value.Recipe = recipe
	if !canonicalSHA256(value.SourceSHA256) {
		return VisualPreviewV1{}, errors.New("visual preview source SHA-256 is invalid")
	}
	switch value.State {
	case VisualPreviewReady:
		if value.Output == nil || value.Failure != nil {
			return VisualPreviewV1{}, errors.New("ready visual preview requires output and no failure")
		}
		output := *value.Output
		value.Output = &output
		if !canonicalSHA256(value.Output.BlobSHA256) {
			return VisualPreviewV1{}, errors.New("visual preview output SHA-256 is invalid")
		}
		if value.Output.Size < 1 {
			return VisualPreviewV1{}, errors.New("visual preview output size must be positive")
		}
		if value.Output.MediaType != recipe.OutputMediaType {
			return VisualPreviewV1{}, errors.New("visual preview output media type does not match recipe")
		}
		if value.Output.Width < 1 || value.Output.Height < 1 ||
			value.Output.Width > recipe.MaxEdgePixels || value.Output.Height > recipe.MaxEdgePixels {
			return VisualPreviewV1{}, errors.New("visual preview dimensions exceed the recipe")
		}
	case VisualPreviewUnsupported, VisualPreviewFailed:
		if value.Output != nil || value.Failure == nil {
			return VisualPreviewV1{}, errors.New("terminal visual preview requires failure and no output")
		}
		failure := *value.Failure
		value.Failure = &failure
		value.Failure.Code = strings.TrimSpace(value.Failure.Code)
		value.Failure.Detail = norm.NFC.String(strings.TrimSpace(value.Failure.Detail))
		if len(value.Failure.Code) > MaxVisualPreviewFailureCode ||
			!visualPreviewFailureCodePattern.MatchString(value.Failure.Code) {
			return VisualPreviewV1{}, errors.New("visual preview failure code is invalid")
		}
		if value.Failure.Detail == "" || len(value.Failure.Detail) > MaxVisualPreviewFailureText ||
			!utf8.ValidString(value.Failure.Detail) {
			return VisualPreviewV1{}, errors.New("visual preview failure detail is invalid")
		}
	default:
		return VisualPreviewV1{}, errors.New("visual preview state is invalid")
	}
	return value, nil
}

func canonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
