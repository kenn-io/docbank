package pdfstamp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

const RecipeContractV1 = "bates-stamp/v1"

var supportedPositions = map[string]bool{
	"top-left": true, "top-center": true, "top-right": true,
	"middle-left": true, "middle-center": true, "middle-right": true,
	"bottom-left": true, "bottom-center": true, "bottom-right": true,
}

type EngineIdentity struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	API     string   `json:"api"`
	Options []string `json:"options"`
}

type Recipe struct {
	Contract       string         `json:"contract"`
	NamespaceID    string         `json:"namespace_id"`
	Prefix         string         `json:"prefix"`
	Suffix         string         `json:"suffix"`
	Padding        int            `json:"padding"`
	StartAt        int            `json:"start_at"`
	Position       string         `json:"position"`
	MarginPoints   int            `json:"margin_points"`
	FontName       string         `json:"font_name"`
	FontSizePoints int            `json:"font_size_points"`
	Color          string         `json:"color"`
	Opacity        float64        `json:"opacity"`
	Units          string         `json:"units"`
	RotationPolicy string         `json:"rotation_policy"`
	Restamp        bool           `json:"restamp"`
	EngineIdentity EngineIdentity `json:"engine_identity"`
}

func (r Recipe) Validate() error {
	r = r.normalized()
	if r.Contract != RecipeContractV1 {
		return fmt.Errorf("invalid Bates stamp recipe contract %q", r.Contract)
	}
	if strings.TrimSpace(r.NamespaceID) == "" {
		return errors.New("bates stamp namespace is required")
	}
	if err := validateLabelPart("prefix", r.Prefix); err != nil {
		return err
	}
	if err := validateLabelPart("suffix", r.Suffix); err != nil {
		return err
	}
	if r.Padding < 1 || r.Padding > 10 {
		return errors.New("bates stamp padding must be between 1 and 10")
	}
	if r.StartAt < 1 || len(strconv.Itoa(r.StartAt)) > r.Padding {
		return errors.New("bates stamp starting sequence does not fit its padding")
	}
	if !supportedPositions[r.Position] {
		return fmt.Errorf("invalid Bates stamp position %q", r.Position)
	}
	if r.MarginPoints < 0 || r.MarginPoints > 144 {
		return errors.New("bates stamp margin must be between 0 and 144 points")
	}
	if r.FontName != "Helvetica" || r.FontSizePoints != 9 {
		return errors.New("bates-stamp/v1 requires 9-point Helvetica")
	}
	if r.Color != "#000000" || r.Opacity != 1 {
		return errors.New("bates-stamp/v1 requires opaque black text")
	}
	if r.Units != "point" {
		return errors.New("bates-stamp/v1 requires point units")
	}
	if r.RotationPolicy != "follow_page" {
		return errors.New("bates-stamp/v1 requires the follow_page rotation policy")
	}
	wantOptions := []string{"onTop=true", "update=restamp"}
	if r.EngineIdentity.Name != "pdfcpu" || r.EngineIdentity.Version != "v0.15.0" ||
		r.EngineIdentity.API != "AddWatermarksMap" || !slices.Equal(r.EngineIdentity.Options, wantOptions) {
		return errors.New("bates-stamp/v1 requires the qualified pdfcpu v0.15.0 engine")
	}
	return nil
}

func (r Recipe) SHA256() (string, error) {
	r = r.normalized()
	if err := r.Validate(); err != nil {
		return "", err
	}
	encoded, err := canonical.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("marshal Bates stamp recipe: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (r Recipe) normalized() Recipe {
	if r.Position == "" {
		r.Position = "bottom-right"
	}
	return r
}

func validateLabelPart(name, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("bates stamp %s is not valid UTF-8", name)
	}
	for _, character := range value {
		if character < 0x20 || character > 0x7e {
			return fmt.Errorf("bates stamp %s contains a character unsupported by Helvetica", name)
		}
		if character == '%' || character == '\\' {
			return fmt.Errorf("bates stamp %s contains a pdfcpu text escape", name)
		}
	}
	return nil
}
