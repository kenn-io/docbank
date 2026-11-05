package main

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// memListener is an in-memory net.Listener. Its connections are net.Pipe
// pairs, so HTTP servers and clients on it block only on channels and timers
// and run on fake time inside a synctest bubble.
type memListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func newMemListener() *memListener {
	return &memListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *memListener) Addr() net.Addr { return memAddr{} }

// isClosed reports whether Close has been called.
func (l *memListener) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

// DialContext connects to the listener, failing once it is closed.
func (l *memListener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
	case <-ctx.Done():
	}
	_ = server.Close()
	_ = client.Close()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, net.ErrClosed
}

// client returns an HTTP client whose every request reaches this listener.
func (l *memListener) client() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: l.DialContext}}
}

type memAddr struct{}

func (memAddr) Network() string { return "memory" }
func (memAddr) String() string  { return "memory" }
