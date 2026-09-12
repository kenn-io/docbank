package emailpdf

import (
	"context"
	"github.com/stretchr/testify/require"
	"io"
	"strings"
	"testing"
)

func TestCDPRejectsWrongResponseAndProtocolErrors(t *testing.T) {
	for _, wire := range []string{`{"id":99,"result":{}}` + "\x00", `{"id":1,"error":{"code":-1,"message":"failure"}}` + "\x00", `not-json` + "\x00"} {
		c := newCDP(strings.NewReader(wire), io.Discard)
		var out struct{}
		require.Error(t, c.call(context.Background(), "Browser.getVersion", nil, &out, ""))
	}
}

func TestCDPLoadWitnessBindsNavigationLoader(t *testing.T) {
	old := `{"method":"Page.lifecycleEvent","sessionId":"selected","params":{"loaderId":"old","name":"load"}}` + "\x00"
	current := `{"method":"Page.lifecycleEvent","sessionId":"selected","params":{"loaderId":"current","name":"load"}}` + "\x00"
	c := newCDP(strings.NewReader(old+current), io.Discard)
	_, err := c.read(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, "current", c.loadedLoader)
	_, err = c.read(t.Context())
	require.NoError(t, err)
	require.Equal(t, "current", c.loadedLoader)
}
