package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapTiles(t *testing.T) {
	require.NoError(t, Default().Web.MapTiles.Validate())
	valid := MapTilesConfig{TileURL: "https://tiles.example.org/{z}/{x}/{y}.png", Attribution: "Example", MaxZoom: 19}
	require.NoError(t, valid.Validate())
	assert.Equal(t, "https://tiles.example.org", valid.Origin())
	canonical := valid
	canonical.TileURL = "https://127.0.0.1/{z}/{x}/{y}.png"
	require.NoError(t, canonical.Validate())
	assert.Equal(t, "https://127.0.0.1", canonical.Origin())
	for _, tc := range []struct{ address, message string }{
		{"https://tiles.example.org/%7Bz%7D/%7Bx%7D/%7By%7D", "tile_url path requires"},
		{"http://tiles.example.org/{z}/{x}/{y}", "tile_url must use a fixed HTTPS origin"},
		{"https://user@tiles.example.org/{z}/{x}/{y}", "tile_url must use a fixed HTTPS origin"},
		{"https://{x}.example.org/{z}/{x}/{y}", "tile_url must use a fixed HTTPS origin"},
		{"https://tiles.example.org/{z}/{x}/{y}#fragment", "tile_url must use a fixed HTTPS origin"},
		{"https://tiles.example.org/{z}/{x}", "tile_url path requires"},
		{"https://*.example.org/{z}/{x}/{y}", "tile_url requires a fixed ASCII hostname"},
		{"https://[2001:db8::1]/{z}/{x}/{y}", "tile_url cannot use an IPv6 address"},
		{"https://127.1/{z}/{x}/{y}.png", "must be a dotted-decimal IPv4 address"},
		{"https://0177.0.0.1/{z}/{x}/{y}.png", "must be a dotted-decimal IPv4 address"},
		{"https://0x7f.0.0.1/{z}/{x}/{y}.png", "must be a dotted-decimal IPv4 address"},
		{"https://0x7f000001/{z}/{x}/{y}.png", "must be a dotted-decimal IPv4 address"},
		{"https://2130706433/{z}/{x}/{y}.png", "must be a dotted-decimal IPv4 address"},
		{"https://127.0.0.1./{z}/{x}/{y}.png", "not a supported IP or DNS authority"},
		{"https://tiles.example.org?/{z}/{x}/{y}", "tile_url path requires"},
		{"https://tiles.example.org:0/{z}/{x}/{y}", "invalid host port"},
		{"https://tiles.example.org/{z}/{x}/{y}/{s}", "tile_url has unsupported placeholders"},
	} {
		t.Run(tc.address, func(t *testing.T) {
			value := valid
			value.TileURL = tc.address
			assert.ErrorContains(t, value.Validate(), tc.message)
		})
	}
	valid.Attribution = ""
	require.ErrorContains(t, valid.Validate(), "attribution is required")
	cfg := Default()
	cfg.Web.MapTiles = valid
	require.ErrorContains(t, cfg.Validate(), "[web.map_tiles]")
	valid.Attribution = "Example"
	for _, zoom := range []int{-1, 23} {
		valid.MaxZoom = zoom
		assert.ErrorContains(t, valid.Validate(), "max_zoom must be between 0 and 22")
	}
}

func TestLoadMapTilesMaxZoom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		setting string
		want    int
	}{{"", 19}, {"max_zoom = 0", 0}} {
		t.Run(tc.setting, func(t *testing.T) {
			t.Parallel()
			dir := privateTestConfigDir(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[web.map_tiles]\ntile_url = \"https://tiles.example.org/{z}/{x}/{y}\"\nattribution = \"Example\"\n"+tc.setting+"\n"), 0o600))
			loaded, err := Load(dir)
			require.NoError(t, err)
			assert.Equal(t, tc.want, loaded.Web.MapTiles.MaxZoom)
		})
	}
}
