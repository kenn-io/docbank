package httpproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listen(t *testing.T, cfg Config) *Proxy {
	t.Helper()
	p, err := Listen(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, p.Close()) })
	return p
}

func newClient(t *testing.T, p *Proxy) *Client {
	t.Helper()
	c, err := p.NewClient()
	require.NoError(t, err)
	return c
}

func bind(t *testing.T, c *Client, ctx context.Context) {
	t.Helper()
	stop, err := c.Bind(ctx)
	require.NoError(t, err)
	t.Cleanup(stop)
}

func host(server *httptest.Server) string {
	return strings.TrimPrefix(strings.TrimPrefix(server.URL, "http://"), "https://")
}

func httpClient(server *httptest.Server, proxy *url.URL) *http.Client {
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxy)
	return &http.Client{Transport: transport}
}

func send(t *testing.T, client *http.Client, target string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)
	return client.Do(request)
}

func get(t *testing.T, client *http.Client, target string) (int, string) {
	t.Helper()
	resp, err := send(t, client, target)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// stall sends one byte of a large body, then holds the response open until
// the client goes away.
func stall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", "1000000")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte{'x'}); err != nil {
		return
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	<-r.Context().Done()
}

func reply(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "hello "+r.URL.Path)
}

func TestForwardsPlainHTTPAndTunnelsTLS(t *testing.T) {
	var sawProxyAuth atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			sawProxyAuth.Store(true)
		}
		_, _ = io.WriteString(w, r.URL.Path+"?"+r.URL.RawQuery)
	})
	plain := httptest.NewServer(handler)
	defer plain.Close()
	secure := httptest.NewTLSServer(handler)
	defer secure.Close()
	p := listen(t, Config{Allow: []string{host(plain), host(secure)}})
	c := newClient(t, p)
	bind(t, c, t.Context())

	for _, server := range []*httptest.Server{plain, secure} {
		status, body := get(t, httpClient(server, c.URL()), server.URL+"/object?prefix=a;b&version=3")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "/object?prefix=a;b&version=3", body, "the proxy forwards the query unchanged")
	}
	assert.False(t, sawProxyAuth.Load(), "proxy credentials must not reach the upstream")
}

func TestRefusesRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(reply))
	defer upstream.Close()
	other := httptest.NewServer(http.HandlerFunc(reply))
	defer other.Close()
	var dials atomic.Int64
	p := listen(t, Config{
		Allow: []string{host(upstream)},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	})
	bound := newClient(t, p)
	bind(t, bound, t.Context())
	unbound := newClient(t, p)
	closed := newClient(t, p)
	bind(t, closed, t.Context())
	require.NoError(t, closed.Close())
	wrongPassword := bound.URL()
	wrongPassword.User = url.UserPassword(bound.username, "wrong")
	anonymous := bound.URL()
	anonymous.User = nil

	tests := []struct {
		name   string
		proxy  *url.URL
		target *httptest.Server
		want   int
	}{
		{"no credentials", anonymous, upstream, http.StatusProxyAuthRequired},
		{"wrong password", wrongPassword, upstream, http.StatusProxyAuthRequired},
		{"closed client", closed.URL(), upstream, http.StatusProxyAuthRequired},
		{"address not allowed", bound.URL(), other, http.StatusForbidden},
		{"unbound client", unbound.URL(), upstream, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _ := get(t, httpClient(tt.target, tt.proxy), tt.target.URL)
			assert.Equal(t, tt.want, status)
			assert.Equal(t, tt.want, connect(t, p, tt.proxy, host(tt.target)))
		})
	}
	assert.Zero(t, dials.Load(), "a refused request must not reach an upstream")
}

// sendConnect opens a connection to the proxy and sends one CONNECT request.
func sendConnect(t *testing.T, p *Proxy, proxy *url.URL, target string) net.Conn {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", p.Addr())
	require.NoError(t, err)
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(proxy.User.Username() + ":" + password))
		request += "Proxy-Authorization: Basic " + credentials + "\r\n"
	}
	_, err = io.WriteString(conn, request+"\r\n")
	require.NoError(t, err)
	return conn
}

// connect sends one CONNECT request and returns the proxy's status.
func connect(t *testing.T, p *Proxy, proxy *url.URL, target string) int {
	t.Helper()
	conn := sendConnect(t, p, proxy, target)
	defer conn.Close()
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

// A stalled transfer would block these tests forever if the cutoff failed.
func TestBindingEndCutsOffTransfers(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(stall))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(stall))
	defer secure.Close()
	p := listen(t, Config{Allow: []string{host(plain), host(secure)}})

	for _, server := range []*httptest.Server{plain, secure} {
		t.Run(server.URL, func(t *testing.T) {
			c := newClient(t, p)
			client := httpClient(server, c.URL())
			ctx, cancel := context.WithCancel(t.Context())
			stop, err := c.Bind(ctx)
			require.NoError(t, err)
			defer stop()

			resp, err := send(t, client, server.URL)
			require.NoError(t, err)
			first := make([]byte, 1)
			_, err = io.ReadFull(resp.Body, first)
			require.NoError(t, err, "the proxy streams the first byte before the cutoff")
			cancel()
			_, err = io.Copy(io.Discard, resp.Body)
			require.NoError(t, resp.Body.Close())
			require.Error(t, err, "the stalled transfer must be cut off")

			resp, err = send(t, client, server.URL)
			if err == nil {
				assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
				require.NoError(t, resp.Body.Close())
			}

			_, err = c.Bind(t.Context())
			require.Error(t, err, "an ended binding stays until stop")
			stop()
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			stop, err = c.Bind(ctx)
			require.NoError(t, err, "a client can bind again after a cutoff")
			defer stop()
			resp, err = send(t, client, server.URL)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
		})
	}
}

func TestStopRefusesNewRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(reply))
	defer upstream.Close()
	p := listen(t, Config{Allow: []string{host(upstream)}})
	c := newClient(t, p)
	client := httpClient(upstream, c.URL())

	stop, err := c.Bind(t.Context())
	require.NoError(t, err)
	_, err = c.Bind(t.Context())
	require.Error(t, err, "a client has one binding at a time")
	status, _ := get(t, client, upstream.URL)
	require.Equal(t, http.StatusOK, status)

	stop()
	status, _ = get(t, client, upstream.URL)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, http.StatusServiceUnavailable, connect(t, p, c.URL(), host(upstream)))
}

func TestCloseEndsTunnels(t *testing.T) {
	secure := httptest.NewTLSServer(http.HandlerFunc(stall))
	defer secure.Close()
	p, err := Listen(Config{Allow: []string{host(secure)}})
	require.NoError(t, err)
	c := newClient(t, p)
	bind(t, c, t.Context())
	resp, err := send(t, httpClient(secure, c.URL()), secure.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.NoError(t, p.Close())
	_, err = io.Copy(io.Discard, resp.Body)
	require.Error(t, err)
	_, err = p.NewClient()
	require.ErrorIs(t, err, ErrClosed)
	_, err = c.Bind(t.Context())
	require.ErrorIs(t, err, ErrClosed)
}

func TestListenRejectsAllowlist(t *testing.T) {
	for _, allow := range [][]string{nil, {"example.com"}, {":443"}, {"example.com:"}} {
		_, err := Listen(Config{Allow: allow})
		assert.Error(t, err, "%q", allow)
	}
}

func TestShutdownClosesUpstreamConnections(t *testing.T) {
	tests := []struct {
		name     string
		shutdown func(p *Proxy, c *Client, cancel context.CancelFunc)
	}{
		{"binding ends", func(_ *Proxy, _ *Client, cancel context.CancelFunc) { cancel() }},
		{"client closes", func(_ *Proxy, c *Client, _ context.CancelFunc) { assert.NoError(t, c.Close()) }},
		{"proxy closes", func(p *Proxy, _ *Client, _ context.CancelFunc) { assert.NoError(t, p.Close()) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closed := make(chan struct{}, 4)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(reply))
			upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}
			upstream.Start()
			defer upstream.Close()
			p := listen(t, Config{Allow: []string{host(upstream)}})
			c := newClient(t, p)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			bind(t, c, ctx)
			status, _ := get(t, httpClient(upstream, c.URL()), upstream.URL)
			require.Equal(t, http.StatusOK, status)

			tt.shutdown(p, c, cancel)
			<-closed
		})
	}
}

// blockingConn holds a cutoff inside Close until the test releases it.
type blockingConn struct {
	net.Conn
	release chan struct{}
}

func (b *blockingConn) Close() error {
	<-b.release
	return nil
}

func TestStopWaitsForStartedCutoff(t *testing.T) {
	p := listen(t, Config{Allow: []string{"192.0.2.1:443"}})
	c := newClient(t, p)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		stop, err := c.Bind(ctx)
		require.NoError(t, err)
		b, err := c.liveBinding()
		require.NoError(t, err)
		held := &blockingConn{release: make(chan struct{})}
		require.True(t, c.track(b, held))

		cancel()
		stopped := make(chan struct{})
		go func() {
			stop()
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			require.FailNow(t, "stop returned while the cutoff was still closing connections")
		default:
		}
		_, err = c.Bind(t.Context())
		require.Error(t, err, "a new binding must wait for the cutoff")

		close(held.release)
		<-stopped
		stop, err = c.Bind(t.Context())
		require.NoError(t, err)
		stop()
	})
}

// After a cutoff, a new binding's request over the same pooled client must
// complete, even though the earlier binding's tunnel was cut.
func TestRebindReusesPooledClient(t *testing.T) {
	more := make(chan struct{})
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		controller := http.NewResponseController(w)
		if _, err := w.Write([]byte{'a'}); err != nil || controller.Flush() != nil {
			return
		}
		select {
		case <-more:
			_, _ = w.Write([]byte{'b'})
		case <-r.Context().Done():
		}
	}))
	defer secure.Close()
	p := listen(t, Config{Allow: []string{host(secure)}})
	c := newClient(t, p)
	client := httpClient(secure, c.URL())
	fetch := func() string {
		resp, err := send(t, client, secure.URL)
		require.NoError(t, err)
		defer resp.Body.Close()
		first := make([]byte, 1)
		_, err = io.ReadFull(resp.Body, first)
		require.NoError(t, err)
		more <- struct{}{}
		rest, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(first) + string(rest)
	}

	ctx, cancel := context.WithCancel(t.Context())
	stop, err := c.Bind(ctx)
	require.NoError(t, err)
	require.Equal(t, "ab", fetch())
	cancel()
	stop()

	bind(t, c, t.Context())
	assert.Equal(t, "ab", fetch())
	assert.Equal(t, "ab", fetch())
}

// Every pending upstream dial, for plain HTTP and for CONNECT, ends when the
// binding ends or stops, or the Client or Proxy closes.
func TestShutdownCancelsPendingDials(t *testing.T) {
	const target = "192.0.2.1:80"
	type binding struct {
		cancel context.CancelFunc
		stop   func()
	}
	shutdowns := []struct {
		name     string
		shutdown func(p *Proxy, c *Client, b binding)
	}{
		{"binding ends", func(_ *Proxy, _ *Client, b binding) { b.cancel() }},
		{"binding stops", func(_ *Proxy, _ *Client, b binding) { b.stop() }},
		{"client closes", func(_ *Proxy, c *Client, _ binding) { assert.NoError(t, c.Close()) }},
		{"proxy closes", func(p *Proxy, _ *Client, _ binding) { assert.NoError(t, p.Close()) }},
	}
	for _, tunnel := range []bool{false, true} {
		for _, tt := range shutdowns {
			t.Run(fmt.Sprintf("tunnel=%t/%s", tunnel, tt.name), func(t *testing.T) {
				dialing := make(chan struct{})
				dialEnded := make(chan struct{})
				p := listen(t, Config{
					Allow: []string{target},
					Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
						close(dialing)
						<-ctx.Done()
						close(dialEnded)
						return nil, ctx.Err()
					},
				})
				c := newClient(t, p)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				stop, err := c.Bind(ctx)
				require.NoError(t, err)
				defer stop()
				if tunnel {
					conn := sendConnect(t, p, c.URL(), target)
					defer conn.Close()
				} else {
					client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(c.URL())}}
					request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+target, nil)
					require.NoError(t, err)
					go func() {
						// The request fails once the dial ends; only the dial matters.
						if resp, err := client.Do(request); err == nil {
							_ = resp.Body.Close()
						}
					}()
				}

				<-dialing
				tt.shutdown(p, c, binding{cancel: cancel, stop: stop})
				<-dialEnded
			})
		}
	}
}

// Concurrent plain-HTTP requests reuse the client's pooled upstream
// connections instead of redialing for each burst.
func TestConcurrentRequestsReuseUpstreamConnections(t *testing.T) {
	const concurrency, bursts = 8, 20
	var opened atomic.Int64
	var gate atomic.Pointer[chan struct{}]
	arrived := make(chan struct{})
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-*gate.Load()
		_, _ = io.WriteString(w, "ok")
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	upstream.Start()
	defer upstream.Close()
	p := listen(t, Config{Allow: []string{host(upstream)}})
	c := newClient(t, p)
	bind(t, c, t.Context())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(c.URL()), MaxIdleConnsPerHost: concurrency}}

	for range bursts {
		open := make(chan struct{})
		gate.Store(&open)
		results := make(chan error, concurrency)
		for range concurrency {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, upstream.URL, nil)
			require.NoError(t, err)
			go func() {
				resp, err := client.Do(request)
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					err = errors.Join(err, resp.Body.Close())
				}
				results <- err
			}()
		}
		for range concurrency {
			<-arrived
		}
		close(open)
		for range concurrency {
			require.NoError(t, <-results)
		}
	}
	// Two idle connections per host would open about (bursts-1)*6+8 = 122.
	// Allow a few redials where a connection returns to the pool late.
	assert.LessOrEqual(t, opened.Load(), int64(2*concurrency))
}
