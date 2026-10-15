package bundle

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func TestPhotoExportSelectionValidate(t *testing.T) {
	valid := PhotoExportSelection{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}
	id := "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name     string
		ids      []string
		badQuery bool
		message  string
	}{
		{"scope", nil, false, ""},
		{"selected", []string{id}, false, ""},
		{"duplicate", []string{id, id}, false, "duplicate photo asset ID"},
		{"too many", make([]string, MaxPhotoExportMembers+1), false, "photo exports allow at most 16 photos"},
		{"invalid ID", []string{"bad"}, false, "photo asset ID must be a canonical UUIDv4"},
		{"uppercase ID", []string{"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"}, false, "photo asset ID must be a canonical UUIDv4"},
		{"wrong version", []string{"11111111-1111-1111-8111-111111111111"}, false, "photo asset ID must be a canonical UUIDv4"},
		{"wrong variant", []string{"11111111-1111-4111-7111-111111111111"}, false, "photo asset ID must be a canonical UUIDv4"},
		{"invalid query", nil, true, "invalid photo query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selection := valid
			selection.AssetIDs = tc.ids
			if tc.badQuery {
				selection.Query.V = 0
			}
			err := selection.Validate()
			if tc.message == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.message)
				if tc.name == "too many" {
					require.ErrorIs(t, err, ErrLimit)
				} else {
					require.ErrorIs(t, err, ErrConflict)
				}
			}
		})
	}
}

func TestPhotoRenderProfileUnmarshalJSON(t *testing.T) {
	for _, tc := range []struct {
		name, raw, message string
		want               PhotoRenderProfile
	}{
		{"null", `{"format":"png","quality":null,"long_edge":0,"include_metadata":true,"remove_gps":true}`, "photo_render.quality must not be null", PhotoRenderProfile{}},
		{"null GPS", `{"format":"png","long_edge":0,"include_metadata":true,"remove_gps":null}`, "photo_render.remove_gps must not be null", PhotoRenderProfile{}},
		{"omitted", `{"format":"png","long_edge":0,"include_metadata":true}`, "photo_render.remove_gps is required", PhotoRenderProfile{}},
		{"PNG metadata off", `{"format":"png","quality":90,"long_edge":2048,"include_metadata":false,"remove_gps":true}`, "", PhotoRenderProfile{Format: "png", LongEdge: 2048}},
		{"JPEG metadata off", `{"format":"jpeg","quality":90,"long_edge":0,"include_metadata":false,"remove_gps":true}`, "", PhotoRenderProfile{Format: "jpeg", Quality: 90}},
		{"JPEG metadata kept", `{"format":"jpeg","quality":90,"long_edge":0,"include_metadata":true,"remove_gps":true}`, "", PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true, RemoveGPS: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var profile PhotoRenderProfile
			err := json.Unmarshal([]byte(tc.raw), &profile)
			if tc.message != "" {
				require.ErrorContains(t, err, tc.message)
				return
			}
			require.NoError(t, err)
			require.NoError(t, profile.Validate())
			require.Equal(t, tc.want, profile.Canonical())
		})
	}
}

func TestPhotoExportReceiptRejectsFutureAndPrivateFields(t *testing.T) {
	profile := PhotoRenderProfile{Format: "png"}
	member := Member{NodeID: 1, VersionID: "40000000-0000-4000-8000-000000000001"}
	receipt := PhotoRenderReceipt{Version: PhotoRenderReceiptVersion, Profile: profile, Source: member, Width: 2, Height: 3}
	role := Role{Role: "photo_rendered", Status: "available", Path: "documents/1/" + member.VersionID + "/photo.png", MediaType: "image/png", SHA256: strings.Repeat("b", 64), Size: 100}
	plan := Plan{Total: 1, PhotoRender: &profile, Roles: []RolePolicy{{Role: "photo_rendered"}}}
	role.Recipe, _ = json.Marshal(receipt)
	_, err := validatePhotoRoles(plan, Document{Member: member, Roles: []Role{role}})
	require.NoError(t, err)
	profile.Quality = 90
	receipt.Profile = profile
	role.Recipe, _ = json.Marshal(receipt)
	validator := RowValidator{Plan: plan}
	err = validator.Add(Document{Member: member, Roles: []Role{role}})
	require.ErrorIs(t, err, ErrConflict)
	profile.Quality = 0
	receipt.Profile = profile
	role.Recipe, _ = json.Marshal(receipt)
	for _, field := range []string{"version", "input", "width"} {
		var fields map[string]any
		require.NoError(t, json.Unmarshal(role.Recipe, &fields))
		switch field {
		case "width":
			fields[field] = int64(1) << 62
			fields["height"] = 4
		case "version":
			fields[field] = PhotoRenderReceiptVersion + 1
		default:
			fields[field] = "Private preparation input"
		}
		bad := role
		bad.Recipe, _ = json.Marshal(fields)
		_, err = validatePhotoRoles(plan, Document{Member: member, Roles: []Role{bad}})
		require.ErrorIs(t, err, ErrConflict)
	}
}
