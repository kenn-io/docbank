package bundle

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoExportReceiptRejectsFutureAndPrivateFields(t *testing.T) {
	profile := PhotoRenderProfile{Format: "png", Quality: 90}
	member := Member{NodeID: 1, VersionID: "40000000-0000-4000-8000-000000000001"}
	receipt := PhotoRenderReceipt{Version: PhotoRenderReceiptVersion, Profile: profile, Source: member, InputSHA256: strings.Repeat("a", 64), Width: 2, Height: 3}
	role := Role{Role: "photo_rendered", Status: "available", Path: "documents/1/" + member.VersionID + "/photo.png", MediaType: "image/png", SHA256: strings.Repeat("b", 64), Size: 100}
	plan := Plan{PhotoRender: &profile, Roles: []RolePolicy{{Role: "photo_rendered"}}}
	role.Recipe, _ = json.Marshal(receipt)
	require.NoError(t, ValidatePhotoRoles(plan, Document{Member: member, Roles: []Role{role}}))
	for _, field := range []string{"version", "input"} {
		var fields map[string]any
		require.NoError(t, json.Unmarshal(role.Recipe, &fields))
		if field == "version" {
			fields[field] = PhotoRenderReceiptVersion + 1
		} else {
			fields[field] = "Private preparation input"
		}
		bad := role
		bad.Recipe, _ = json.Marshal(fields)
		require.ErrorIs(t, ValidatePhotoRoles(plan, Document{Member: member, Roles: []Role{bad}}), ErrConflict)
	}
}
