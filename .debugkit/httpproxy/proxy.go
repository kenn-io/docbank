// Package httpproxy runs a loopback HTTP forward proxy that lets a Go program
// bound the network traffic of code it cannot cancel directly, such as an
// embedded C library or a child process that only accepts a proxy setting.
//
// Each Client has its own random proxy credentials. A Client starts new
// requests, tunnels, and upstream dials only while it is bound to a context.
// When that context ends, the proxy closes every connection the Client holds,
// which stops transfers already in flight, including traffic inside
// established tunnels. Only allowlisted upstream addresses are reachable, so
// other local processes cannot use the proxy as a relay.
package httpproxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrClosed reports use of a closed Proxy or Client.
var ErrClosed = errors.New("httpproxy: closed")

var errUnbound = errors.New("httpproxy: client is not bound")

// Config configures a Proxy.
type Config struct {
	// Allow lists the upstream "host:port" addresses that clients may reach,
	// through CONNECT for TLS or absolute-form requests for plain HTTP. Hosts
	// compare case-insensitively. At least one address is required.
	Allow []string
	// Dial opens upstream connections. Nil uses a zero net.Dialer.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// Proxy is a forward proxy listening on a loopback address.
type Proxy struct {
	listener net.Listener
	server   *http.Server
	allow    map[string]bool
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
	buffers  *bufferPool
	serving  chan error
	tunnels  sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	clients map[string]*Client
	owners  map[net.Conn]*Client
}

// Client is one caller's identity on a Proxy.
type Client struct {
	proxy    *Proxy
	username string
	password string
	// forward has the Client's own upstream pool, whose connections are
	// dialed and tracked under the Client's binding.
	forward *httputil.ReverseProxy
	// life ends when the Client closes, cancelling its pending dials.
	life    context.Context
	endLife context.CancelFunc

	// Guarded by proxy.mu.
	closed  bool
	binding *binding
	conns   map[net.Conn]struct{}
}

// binding stays the Client's binding after its context ends, until stop. No
// newer binding can exist while a cutoff runs, so a cutoff may close every
// connection the Client holds.
type binding struct {
	ctx context.Context
	// active ends with ctx or at stop, cancelling the binding's pending dials
	// without touching its established connections.
	active     context.Context
	deactivate context.CancelFunc
	// cancelCutoff reports true when it prevented the cutoff from starting.
	cancelCutoff func() bool
	// ended is closed once no cutoff for this binding can still run.
	ended chan struct{}
}

type connKey struct{}

// Listen starts a Proxy on 127.0.0.1 with an ephemeral port.
func Listen(cfg Config) (*Proxy, error) {
	allow, err := allowlist(cfg.Allow)
	if err != nil {
		return nil, err
	}
	dial := cfg.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("httpproxy: listen on loopback: %w", err)
	}
	p := &Proxy{
		listener: listener,
		allow:    allow,
		dial:     dial,
		buffers:  &bufferPool{},
		serving:  make(chan error, 1),
		clients:  make(map[string]*Client),
		owners:   make(map[net.Conn]*Client),
	}
	p.server = &http.Server{
		Handler:           http.HandlerFunc(p.serveHTTP),
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, connKey{}, conn)
		},
		ConnState: func(conn net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				p.untrack(conn)
			}
		},
	}
	go func() { p.serving <- p.server.Serve(listener) }()
	return p, nil
}

func allowlist(addresses []string) (map[string]bool, error) {
	if len(addresses) == 0 {
		return nil, errors.New("httpproxy: Config.Allow must name at least one upstream address")
	}
	allow := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("httpproxy: allowed address %q must be host:port", address)
		}
		allow[net.JoinHostPort(strings.ToLower(host), port)] = true
	}
	return allow, nil
}

// Addr returns the proxy's "127.0.0.1:port" listening address.
func (p *Proxy) Addr() string { return p.listener.Addr().String() }

// NewClient creates a Client with random credentials. The Client is unbound.
func (p *Proxy) NewClient() (*Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	life, endLife := context.WithCancel(context.Background())
	c := &Client{
		proxy:    p,
		username: rand.Text(),
		password: rand.Text(),
		life:     life,
		endLife:  endLife,
		conns:    make(map[net.Conn]struct{}),
	}
	c.forward = &httputil.ReverseProxy{
		// The inbound request already carries the absolute upstream URL.
		// Restore the raw query, which ReverseProxy would otherwise strip
		// of parameters it cannot parse.
		Rewrite: func(r *httputil.ProxyRequest) { r.Out.URL.RawQuery = r.In.URL.RawQuery },
		Transport: &http.Transport{
			DialContext: c.dialBound,
			// Keep every idle connection. The client's own concurrency bounds
			// the pool, and a cutoff or Close ends its connections; Go's
			// default of two per host would redial for each concurrent burst.
			MaxIdleConnsPerHost: math.MaxInt,
		},
		// Stream each upstream read at once; a slow response must reach the
		// client as it arrives, not when a buffer fills.
		FlushInterval: -1,
		BufferPool:    p.buffers,
		ErrorHandler:  forwardError,
	}
	p.clients[c.username] = c
	return c, nil
}

// Close stops the proxy, closes every connection, and waits for open tunnels
// to end. Clients become unusable.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	var conns []net.Conn
	for _, c := range p.clients {
		conns = append(conns, c.release()...)
	}
	p.mu.Unlock()
	closeAll(conns)
	err := p.server.Close()
	p.tunnels.Wait()
	if serveErr := <-p.serving; !errors.Is(serveErr, http.ErrServerClosed) {
		err = errors.Join(err, serveErr)
	}
	return err
}

// URL returns the proxy URL with this Client's credentials, in the form
// http://username:password@127.0.0.1:port.
func (c *Client) URL() *url.URL {
	return &url.URL{Scheme: "http", User: url.UserPassword(c.username, c.password), Host: c.proxy.Addr()}
}

// Bind lets the Client start requests, tunnels, and upstream dials until ctx
// ends or stop is called. When ctx ends first, the proxy closes every
// connection the Client holds.
//
// stop refuses new requests, tunnels, and dials, cancels pending dials, and
// keeps established connections for reuse by a later Bind. The proxy cannot see inside a tunnel, so traffic
// over an established tunnel continues after stop until a later binding's
// context ends or the Client or Proxy closes. If a cutoff has started, stop
// waits for it to finish. A Client has one binding at a time: call stop,
// even after ctx ends, before binding again.
func (c *Client) Bind(ctx context.Context) (stop func(), err error) {
	p := c.proxy
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.binding != nil {
		return nil, errors.New("httpproxy: client is already bound; call stop first")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	active, deactivate := context.WithCancel(ctx)
	b := &binding{ctx: ctx, active: active, deactivate: deactivate, ended: make(chan struct{})}
	c.binding = b
	b.cancelCutoff = context.AfterFunc(ctx, func() {
		c.cutOff()
		close(b.ended)
	})
	return sync.OnceFunc(func() {
		b.deactivate()
		if b.cancelCutoff() {
			close(b.ended)
		}
		<-b.ended
		p.mu.Lock()
		defer p.mu.Unlock()
		if c.binding == b {
			c.binding = nil
		}
	}), nil
}

// Close revokes the Client's credentials and closes its connections.
func (c *Client) Close() error {
	p := c.proxy
	p.mu.Lock()
	if c.closed {
		p.mu.Unlock()
		return nil
	}
	delete(p.clients, c.username)
	conns := c.release()
	p.mu.Unlock()
	closeAll(conns)
	return nil
}

// release marks c closed, cancels its pending dials, and detaches its
// connections. p.mu must be held.
func (c *Client) release() []net.Conn {
	c.closed = true
	c.endLife()
	if b := c.binding; b != nil {
		b.deactivate()
		if b.cancelCutoff() {
			close(b.ended)
		}
		c.binding = nil
	}
	return c.detachConns()
}

// detachConns removes c's connections from tracking. p.mu must be held.
func (c *Client) detachConns() []net.Conn {
	conns := make([]net.Conn, 0, len(c.conns))
	for conn := range c.conns {
		conns = append(conns, conn)
		delete(c.proxy.owners, conn)
	}
	clear(c.conns)
	return conns
}

func (c *Client) cutOff() {
	p := c.proxy
	p.mu.Lock()
	conns := c.detachConns()
	p.mu.Unlock()
	closeAll(conns)
}

// liveBinding returns c's binding if its context has not ended.
func (c *Client) liveBinding() (*binding, error) {
	p := c.proxy
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.binding == nil || c.binding.ctx.Err() != nil {
		return nil, errUnbound
	}
	return c.binding, nil
}

// track records conn as c's while b is c's live binding. It reports false
// when the binding ended or another Client owns conn, so the caller must not
// use conn. A binding's context ends before its cutoff runs, so every
// connection tracked under b is closed by b's cutoff.
func (c *Client) track(b *binding, conn net.Conn) bool {
	p := c.proxy
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.binding != b || b.ctx.Err() != nil {
		return false
	}
	if owner, ok := p.owners[conn]; ok && owner != c {
		return false
	}
	c.conns[conn] = struct{}{}
	p.owners[conn] = c
	return true
}

func (p *Proxy) untrack(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.owners[conn]; ok {
		delete(c.conns, conn)
		delete(p.owners, conn)
	}
}

// closeAll closes connections. p.mu must not be held: a tracked upstream
// connection untracks itself on close.
func closeAll(conns []net.Conn) {
	for _, conn := range conns {
		// The connection is being abandoned; its close result changes nothing.
		_ = conn.Close()
	}
}

// dial opens an upstream connection under binding b. The dial stops when b
// ends or stops, or the Client closes, and the connection is tracked under b.
func (c *Client) dial(ctx context.Context, b *binding, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(b.active, cancel)()
	defer context.AfterFunc(c.life, cancel)()
	raw, err := c.proxy.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	conn := &upstreamConn{Conn: raw, proxy: c.proxy}
	if !c.track(b, conn) {
		_ = raw.Close()
		return nil, errUnbound
	}
	return conn, nil
}

// dialBound dials for the Client's upstream pool under its current binding.
// The pool keeps dialing after a request is cancelled, so the binding, not
// the request, must bound the dial.
func (c *Client) dialBound(ctx context.Context, network, address string) (net.Conn, error) {
	b, err := c.liveBinding()
	if err != nil {
		return nil, err
	}
	return c.dial(ctx, b, network, address)
}

// upstreamConn stops being tracked when closed, whoever closes it.
type upstreamConn struct {
	net.Conn
	proxy *Proxy
}

func (u *upstreamConn) Close() error {
	u.proxy.untrack(u)
	return u.Conn.Close()
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	c, b, err := p.authorize(r)
	if err != nil {
		status := http.StatusInternalServerError
		if refused, ok := errors.AsType[refusal](err); ok {
			status = int(refused)
		}
		if status == http.StatusProxyAuthRequired {
			w.Header().Set("Proxy-Authenticate", `Basic realm="httpproxy"`)
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	inbound, _ := r.Context().Value(connKey{}).(net.Conn)
	if inbound == nil || !c.track(b, inbound) {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(b.ctx, cancel)()
	if r.Method == http.MethodConnect {
		p.tunnel(ctx, w, r, c, b)
		return
	}
	c.forward.ServeHTTP(w, r.WithContext(ctx))
}

// refusal is the HTTP status the proxy answers a refused request with.
type refusal int

func (r refusal) Error() string { return "httpproxy: " + http.StatusText(int(r)) }

// authorize returns the request's Client and its live binding, or a refusal.
func (p *Proxy) authorize(r *http.Request) (*Client, *binding, error) {
	username, password, ok := proxyCredentials(r.Header.Get("Proxy-Authorization"))
	if !ok {
		return nil, nil, refusal(http.StatusProxyAuthRequired)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.clients[username]
	if c == nil || subtle.ConstantTimeCompare([]byte(password), []byte(c.password)) != 1 {
		return nil, nil, refusal(http.StatusProxyAuthRequired)
	}
	address, ok := upstreamAddress(r)
	if !ok || !p.allow[address] {
		return nil, nil, refusal(http.StatusForbidden)
	}
	if c.binding == nil || c.binding.ctx.Err() != nil {
		return nil, nil, refusal(http.StatusServiceUnavailable)
	}
	return c, c.binding, nil
}

func proxyCredentials(header string) (username, password string, ok bool) {
	scheme, encoded, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", false
	}
	return strings.Cut(string(decoded), ":")
}

// upstreamAddress returns the normalized host:port a request targets.
func upstreamAddress(r *http.Request) (string, bool) {
	authority := r.Host
	if r.Method != http.MethodConnect {
		if r.URL.Scheme != "http" || r.URL.Host == "" {
			return "", false
		}
		authority = r.URL.Host
		if r.URL.Port() == "" {
			authority = net.JoinHostPort(r.URL.Hostname(), "80")
		}
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return "", false
	}
	return net.JoinHostPort(strings.ToLower(host), port), true
}

func (p *Proxy) tunnel(ctx context.Context, w http.ResponseWriter, r *http.Request, c *Client, b *binding) {
	address, _ := upstreamAddress(r)
	upstream, err := c.dial(ctx, b, "tcp", address)
	if err != nil {
		http.Error(w, "dial upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	inbound, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = upstream.Close()
		http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Hijacking hides the connection from the server, so the tunnel owns it.
	// Starting under p.mu orders the start before Close waits for tunnels.
	p.mu.Lock()
	started := !p.closed
	if started {
		p.tunnels.Go(func() {
			defer p.untrack(inbound)
			defer inbound.Close()
			defer upstream.Close()
			if _, err := io.WriteString(inbound, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
				return
			}
			splice(inbound, buffered.Reader, upstream)
		})
	}
	p.mu.Unlock()
	if !started {
		_ = inbound.Close()
		_ = upstream.Close()
	}
}

// splice copies both directions until either side ends, then closes both.
func splice(inbound net.Conn, fromClient *bufio.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, fromClient)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(inbound, upstream)
		done <- struct{}{}
	}()
	<-done
	_ = inbound.Close()
	_ = upstream.Close()
	<-done
}

func forwardError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway
	if r.Context().Err() != nil {
		status = http.StatusServiceUnavailable
	}
	http.Error(w, err.Error(), status)
}

// bufferPool reuses ReverseProxy copy buffers across responses.
type bufferPool struct{ pool sync.Pool }

func (b *bufferPool) Get() []byte {
	if buf, ok := b.pool.Get().(*[]byte); ok {
		return *buf
	}
	return make([]byte, 32<<10)
}

func (b *bufferPool) Put(buf []byte) { b.pool.Put(&buf) }
