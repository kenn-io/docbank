package httpboundary

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAllowedAuthorities(t *testing.T) {
	for _, test := range []struct {
		host string
		want bool
	}{
		{"docbank:8485", true}, {"DOCBANK:8485", true}, {"docbank:8486", false},
		{"archive.example:9999", true}, {"192.0.2.10:8485", true},
		{"[::ffff:192.0.2.10]:8485", true},
		{"127.0.0.1:8485", true}, {"localhost:8485", true}, {"[::1]:8485", true},
		{"[::ffff:127.0.0.1]:8485", true}, {"unconfigured.example:8485", false},
		{"0.0.0.0:8485", false}, {"[::ffff:0.0.0.0]:8485", false}, {"[::]:8485", false}, {"docbank:0", false},
		{"docbank:", false}, {"docbank:65536", false}, {"docbank:abc", false},
		{"user@docbank:8485", false}, {"docbank:8485/path", false}, {"docbank:8485\r\n", false},
		{"[::1%eth0]:8485", false}, {"*.example", false}, {"docbank..example", false},
	} {
		t.Run(test.host, func(t *testing.T) {
			assert.Equal(t, test.want, Allowed(test.host, "192.0.2.10", []string{"docbank:8485", "archive.example"}))
		})
	}
	assert.False(t, Allowed("unconfigured.example:8485", "0.0.0.0", nil))
	assert.False(t, Allowed("docbank:8485", "::", nil))
	assert.True(t, Same("DOCBANK", "docbank:80"))
	assert.False(t, Same("docbank:8485", "docbank:8486"))
	assert.False(t, Same("127.0.0.1:7341", "[::ffff:127.0.0.1]:7341"))
	assert.True(t, Same("[::ffff:127.0.0.1]:7341", "[::ffff:7f00:1]:7341"))
}
