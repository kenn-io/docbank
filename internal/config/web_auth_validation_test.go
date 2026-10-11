package config

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/httpboundary"
	"testing"
	"time"
)

const syntheticPublicKey = "synthetic-key-0123456789abcdef0123456789"

func TestWebLoginRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing key", func(c *Config) { c.Server.APIKey = "" }},
		{"short key", func(c *Config) { c.Server.APIKey = syntheticPublicKey[:31] }},
		{"disabled app", func(c *Config) { c.Web.Enabled = false }},
		{"public HTTP", func(c *Config) { c.Web.PublicOrigin = "http://archive.example.test" }},
		{"userinfo", func(c *Config) { c.Web.PublicOrigin = "https://secret@archive.example.test" }},
		{"path", func(c *Config) { c.Web.PublicOrigin = "https://archive.example.test/" }},
		{"query", func(c *Config) { c.Web.PublicOrigin = "https://archive.example.test?x=y" }},
		{"fragment", func(c *Config) { c.Web.PublicOrigin = "https://archive.example.test#" }},
		{"wildcard", func(c *Config) { c.Web.AllowedHosts = []string{"*.example.test"} }},
		{"trailing-dot origin", func(c *Config) { c.Web.PublicOrigin = "https://archive.example.test." }},
		{"trailing-dot host", func(c *Config) { c.Web.AllowedHosts = []string{"backend.example.test.:7777"} }},
		{"scheme in host", func(c *Config) { c.Web.AllowedHosts = []string{"https://archive.example.test"} }},
		{"bad port", func(c *Config) { c.Web.AllowedHosts = []string{"localhost:65536"} }},
		{"short lifetime", func(c *Config) { c.Web.SessionLifetime = Duration(time.Second) }},
		{"long lifetime", func(c *Config) { c.Web.SessionLifetime = Duration(91 * 24 * time.Hour) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			c := Default()
			c.Server.APIKey = syntheticPublicKey
			c.Web.PublicOrigin = "https://archive.example.test"
			test.mutate(&c)
			require.Error(t, c.Validate())
		})
	}
	c := Default()
	c.Server.APIKey = syntheticPublicKey
	c.Web.PublicOrigin = "http://archive.example.test"
	c.Web.TrustPrivateNetwork = true
	require.NoError(t, c.Validate())
}

func FuzzWebHostAuthorityRejectsPatternsAndURLs(f *testing.F) {
	f.Add("localhost:7777")
	f.Add("*.example.test")
	f.Add("https://archive.example.test")
	f.Add("archive.example.test:65536")
	f.Add("archive.example.test.")
	f.Add("under_score.example.test")
	f.Add("[::ffff:127.0.0.1]:7777")
	f.Add("[2001:0db8::0001]:08080")
	f.Fuzz(func(t *testing.T, host string) {
		authority, err := WebHostAuthority(host)
		if err == nil {
			require.NotContains(t, authority, "*")
			require.NotContains(t, authority, "/")
			require.NotContains(t, authority, "@")
			require.NotContains(t, authority, "?")
			require.True(t, httpboundary.Allowed(authority, "", []string{authority}),
				"the server Host allowlist must admit accepted authority %q", authority)
		}
	})
}
