package providerhttp

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fallbackResolver struct{}

func (fallbackResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.1")}, nil
}

func TestDialFallsBackInDNSOrder(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	firstAddress := net.JoinHostPort("::1", strconv.Itoa(port))
	first, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp6", firstAddress)
	if err != nil {
		probe, probeErr := (&net.ListenConfig{}).Listen(t.Context(), "tcp6", "[::1]:0")
		if probeErr != nil {
			t.Skipf("IPv6 loopback unavailable: %v", probeErr)
		}
		require.NoError(t, probe.Close())
		require.NoError(t, err)
	}
	t.Cleanup(func() { _ = first.Close() })

	policy, err := validatePolicy(EgressPolicy{
		Scheme: "http", Host: "provider.invalid", Port: uint16(port),
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("127.0.0.0/8")},
	})
	require.NoError(t, err)
	var attempts []string
	dialer := policyDialer{policy: policy, resolver: fallbackResolver{}, dialer: net.Dialer{
		Control: func(_, address string, _ syscall.RawConn) error {
			attempts = append(attempts, address)
			return nil
		},
	}}
	require.NoError(t, first.Close())
	connection, err := dialer.dial(t.Context(), "tcp", net.JoinHostPort("provider.invalid", strconv.Itoa(port)))
	require.NoError(t, err)
	require.NoError(t, connection.Close())
	assert.Equal(t, []string{firstAddress, listener.Addr().String()}, attempts)
}
