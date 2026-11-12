package main

import (
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/home"
	docweb "go.kenn.io/docbank/internal/web"
)

// daemonProcess owns what lets clients find the daemon for the life of the
// process: the vault lock, runtime record, listeners, credentials, and
// browser sessions. A browser restart ends one serve run and starts another
// against the same daemonProcess, so the CLI never sees the daemon go away
// and the launch lock is never involved. Everything else belongs to a run.
type daemonProcess struct {
	root    *os.Root
	lock    *home.Lock
	vault   string
	recPath string

	api, local, web *sharedListener
	addr, webURL    string

	shutdownToken   string
	apiKey          string
	webLoginEnabled bool

	sessions api.WebSessionHandoff
	restart  atomic.Bool
}

// acquire takes the vault on the first run. Later runs already hold it.
func (p *daemonProcess) acquire(layout home.Layout) error {
	if p.lock != nil {
		return nil
	}
	root, lock, err := layout.OpenAndLockExclusive()
	if err != nil {
		return err
	}
	p.root, p.lock, p.vault = root, lock, layout.Root
	return docweb.RemoveBootstrap(layout.Root)
}

// credentials fixes the record's shutdown token and API key on the first run.
// A configured key is used as-is; otherwise a fresh per-process key is
// generated and published only to same-user clients via the runtime record
// inside owner-private DOCBANK_HOME — never over the network, never logged.
func (p *daemonProcess) credentials(configured string) error {
	if p.shutdownToken != "" {
		return nil
	}
	token, err := randomHex32()
	if err != nil {
		return fmt.Errorf("generating shutdown token: %w", err)
	}
	key := configured
	if key == "" {
		if key, err = randomHex32(); err != nil {
			return fmt.Errorf("generating ephemeral API key: %w", err)
		}
	}
	p.shutdownToken, p.apiKey, p.webLoginEnabled = token, key, configured != ""
	return nil
}

// close releases the process-owned resources in the order a single run used
// to: listeners, bootstrap, lock, root, and the runtime record last.
func (p *daemonProcess) close() {
	for _, l := range []*sharedListener{p.web, p.local, p.api} {
		if l != nil {
			l.close()
		}
	}
	if p.lock == nil {
		return
	}
	removeWebBootstrap(p.vault)
	_ = p.lock.Release()
	_ = p.root.Close()
	if p.recPath != "" {
		_ = os.Remove(p.recPath)
	}
}

// sharedListener keeps one bound socket across serve runs. Each run's
// http.Server serves a view; closing the view stops that server from
// accepting without unbinding the port, so connections that arrive during a
// restart wait for the next run instead of being refused.
type sharedListener struct {
	net.Listener

	accepted chan accepted
	done     chan struct{}

	mu       sync.Mutex
	requeued []net.Conn
}

func shareListener(l net.Listener) *sharedListener {
	s := &sharedListener{Listener: l, accepted: make(chan accepted), done: make(chan struct{})}
	go s.acceptLoop()
	return s
}

type accepted struct {
	conn net.Conn
	err  error
}

// acceptLoop hands each result to whichever run is serving. Errors pass
// through so http.Server applies its usual backoff or stops serving.
func (s *sharedListener) acceptLoop() {
	for {
		conn, err := s.Accept()
		select {
		case s.accepted <- accepted{conn, err}:
		case <-s.done:
			if conn != nil {
				_ = conn.Close()
			}
			return
		}
	}
}

// view returns a listener for one run. Its Close leaves the socket bound.
func (s *sharedListener) view() net.Listener {
	if s == nil {
		return nil
	}
	return &listenerView{shared: s, closed: make(chan struct{})}
}

func (s *sharedListener) close() {
	close(s.done)
	_ = s.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.requeued {
		_ = conn.Close()
	}
	s.requeued = nil
}

type listenerView struct {
	shared *sharedListener
	once   sync.Once
	closed chan struct{}
}

func (v *listenerView) Accept() (net.Conn, error) {
	s := v.shared
	select {
	case <-v.closed:
		return nil, net.ErrClosed
	default:
	}
	s.mu.Lock()
	if len(s.requeued) > 0 {
		conn := s.requeued[0]
		s.requeued = s.requeued[1:]
		s.mu.Unlock()
		return conn, nil
	}
	s.mu.Unlock()
	select {
	case result := <-s.accepted:
		select {
		case <-v.closed:
			if result.conn != nil {
				// Close raced this receive; hand the connection to the next run.
				s.mu.Lock()
				s.requeued = append(s.requeued, result.conn)
				s.mu.Unlock()
			}
			return nil, net.ErrClosed
		default:
			return result.conn, result.err
		}
	case <-v.closed:
		return nil, net.ErrClosed
	case <-s.done:
		return nil, net.ErrClosed
	}
}

func (v *listenerView) Close() error {
	v.once.Do(func() { close(v.closed) })
	return nil
}

func (v *listenerView) Addr() net.Addr { return v.shared.Addr() }

// restartRequested reports and clears a pending browser restart.
func (p *daemonProcess) restartRequested() bool {
	return p.restart.Swap(false)
}
