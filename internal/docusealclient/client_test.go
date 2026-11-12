package docusealclient

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"go.kenn.io/docbank/internal/config"
)

func TestConnectionChecksAndListsTemplates(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != "synthetic-key" {
			t.Error("missing credential")
		}
		switch r.URL.Path {
		case "/api/user":
			_, _ = fmt.Fprint(w, `{"id":7,"email":"private@example.test"}`)
		case "/api/templates":
			_, _ = fmt.Fprint(w, `{"data":[{"id":12,"name":"Lease","submitters":[{"name":"Tenant"}]}],"pagination":{"next":12}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIPath: "/api", APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := c.Templates(t.Context(), 0)
	if err != nil || len(page.Templates) != 1 || len(page.Templates[0].Roles) != 1 || page.Templates[0].Roles[0] != "Tenant" || page.Next != 0 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}
func TestConnectionRejectsMalformedProviderShapes(t *testing.T) {
	for _, body := range []string{`{"id":0}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer s.Close()
			c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Check(t.Context()); err == nil {
				t.Fatal("accepted invalid user response")
			}
		})
	}
	for _, body := range []string{`{}`, `{"data":null}`, `{"Data":[]}`, `{"data":[],"data":[]}`, `{"data":[{"id":1,"submitters":[]}]}`, `{"data":[{"id":1,"name":null,"submitters":[]}]}`, `{"data":[{"name":"Lease","submitters":[]}]}`, `{"data":[{"id":null,"name":"Lease","submitters":[]}]}`, `{"data":[{"id":1,"name":"Lease","submitters":[{}]}]}`, `{"data":[{"id":1,"name":"Lease","submitters":[{"name":null}]}]}`, `{"data":[{"id":1,"name":"Lease","submitters":[null]}]}`} {
		t.Run("templates/"+body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer s.Close()
			c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Templates(t.Context(), 0); err == nil {
				t.Fatal("accepted invalid template page")
			}
		})
	}
}

func TestConnectionRejectsCompressedOversize(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				w.WriteHeader(status)
				z := gzip.NewWriter(w)
				_, _ = io.WriteString(z, strings.Repeat("x", maxResponse+1))
				_ = z.Close()
			}))
			defer s.Close()
			c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
			if err != nil {
				t.Fatal(err)
			}
			want := "docuseal_response_too_large"
			if status != http.StatusOK {
				want = "docuseal_provider_error"
			}
			if _, err = c.Check(t.Context()); err == nil || err.Error() != want {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestConnectionDeniesAddressOutsideAllowlist(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("denied destination reached") }))
	defer s.Close()
	c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Check(t.Context()); err == nil {
		t.Fatal("accepted denied address")
	}
}

func TestTemplatesContinuationAndUnsafeText(t *testing.T) {
	for _, count := range []int{0, 1, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("after") != "200" || r.URL.Query().Get("limit") != "100" {
					t.Error("incorrect pagination query")
				}
				var items []string
				for i := range count {
					items = append(items, fmt.Sprintf(`{"id":%d,"name":"A\u001b%s","submitters":[%s]}`, 199-i, strings.Repeat("é", 300), strings.TrimSuffix(strings.Repeat(`{"name":"Signer"},`, 21), ",")))
				}
				next := "null"
				if count > 0 {
					next = strconv.Itoa(200 - count)
				}
				_, _ = fmt.Fprintf(w, `{"data":[%s],"pagination":{"next":%s}}`, strings.Join(items, ","), next)
			}))
			defer s.Close()
			c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
			if err != nil {
				t.Fatal(err)
			}
			page, err := c.Templates(t.Context(), 200)
			if err != nil || len(page.Templates) != count {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			if count == 100 && page.Next != 100 || count < 100 && page.Next != 0 {
				t.Fatalf("next=%d", page.Next)
			}
			if count > 0 && (len(page.Templates[0].Name) > 512 || strings.ContainsRune(page.Templates[0].Name, '\x1b') || len(page.Templates[0].Roles) != 21) {
				t.Fatalf("unsafe projection: %+v", page.Templates[0])
			}
		})
	}
	// A credential in a template name must not be projected.
	cName := &Client{connection: config.DocuSealConnection{Endpoint: "https://example.test", APIKey: "synthetic-key"}, http: &http.Client{Transport: docusealRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":1,"name":"synthetic-key","submitters":[]}]}`))}, nil
	})}}
	if _, err := cName.Templates(t.Context(), 0); err == nil {
		t.Fatal("accepted credential in template name")
	}
	// Role identities cannot be rewritten; reject an echoed credential.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":1,"name":"ok","submitters":[{"name":"synthetic-key"}]}]}`)
	}))
	defer s.Close()
	c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Templates(t.Context(), 0); err == nil || strings.Contains(err.Error(), "synthetic-key") {
		t.Fatalf("unsafe role error: %v", err)
	}
}

func TestTemplatesOversizedPages(t *testing.T) {
	t.Run("reducible", func(t *testing.T) {
		var requests []string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests = append(requests, r.URL.RawQuery)
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			after := r.URL.Query().Get("after")
			if after == "51" {
				_, _ = fmt.Fprint(w, `{"data":[{"id":50,"name":"Last","submitters":[]}],"pagination":{"next":50}}`)
				return
			}
			if limit > 50 {
				_, _ = io.WriteString(w, strings.Repeat("x", maxResponse+1))
				return
			}
			_, _ = fmt.Fprint(w, `{"data":[`)
			for i := range limit {
				if i > 0 {
					_, _ = fmt.Fprint(w, ",")
				}
				_, _ = fmt.Fprintf(w, `{"id":%d,"name":"Template","submitters":[],"fields":"%s"}`, 100-i, strings.Repeat("x", 1000))
			}
			_, _ = fmt.Fprint(w, `],"pagination":{"next":51}}`)
		}))
		defer s.Close()
		c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseIdleConnections()
		page, err := c.Templates(t.Context(), 0)
		if err != nil || len(page.Templates) != 50 || page.Next != 51 {
			t.Fatalf("page=%+v err=%v", page, err)
		}
		page, err = c.Templates(t.Context(), page.Next)
		if err != nil || len(page.Templates) != 1 || page.Next != 0 {
			t.Fatalf("final=%+v err=%v", page, err)
		}
		if len(requests) != 3 || requests[0] != "limit=100" || requests[1] != "limit=50" || requests[2] != "limit=100&after=51" {
			t.Fatalf("requests: %v", requests)
		}
	})
	t.Run("irreducible", func(t *testing.T) {
		var limits []string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limits = append(limits, r.URL.Query().Get("limit"))
			_, _ = io.WriteString(w, strings.Repeat("x", maxResponse+1))
		}))
		defer s.Close()
		c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseIdleConnections()
		_, err = c.Templates(t.Context(), 0)
		if err == nil || err.Error() != "docuseal_response_too_large" || strings.Join(limits, ",") != "100,50,25,12,6,3,1" {
			t.Fatalf("limits=%v err=%v", limits, err)
		}
	})
}

type docusealRoundTripFunc func(*http.Request) (*http.Response, error)

func (f docusealRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type docusealErrorBody struct{ err error }

func (b docusealErrorBody) Read([]byte) (int, error) { return 0, b.err }
func (b docusealErrorBody) Close() error             { return nil }

func TestCheckPreservesBodyContextErrors(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		c := &Client{connection: config.DocuSealConnection{Endpoint: "https://example.test", APIKey: "synthetic-key"}, http: &http.Client{Transport: docusealRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: docusealErrorBody{err: want}}, nil
		})}}
		_, err := c.Check(t.Context())
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	}
}

func TestCheckRedactsKeyInEndpoint(t *testing.T) {
	c := &Client{connection: config.DocuSealConnection{Endpoint: "https://key.example.test", APIKey: "key"}, http: &http.Client{Transport: docusealRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":1}`))}, nil
	})}}
	status, err := c.Check(t.Context())
	if err != nil || strings.Contains(status.Endpoint, "key") || status.Endpoint != "*" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestTemplatesRequiresSubmitters(t *testing.T) {
	for _, tc := range []struct {
		field string
		valid bool
	}{{"", false}, {`,"submitters":null`, false}, {`,"submitters":[]`, true}} {
		c := &Client{connection: config.DocuSealConnection{Endpoint: "https://example.test", APIKey: "synthetic-key"}, http: &http.Client{Transport: docusealRoundTripFunc(func(*http.Request) (*http.Response, error) {
			body := `{"data":[{"id":1,"name":"Lease"` + tc.field + `}]}`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		_, err := c.Templates(t.Context(), 0)
		if (err == nil) != tc.valid {
			t.Fatalf("field %q: %v", tc.field, err)
		}
	}
}

func TestTemplatesAggregateDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		start := time.Now()
		c := &Client{connection: config.DocuSealConnection{Endpoint: "https://example.test", APIKey: "synthetic-key"}, http: &http.Client{Transport: docusealRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			select {
			case <-time.After(9 * time.Second):
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxResponse+1)))}, nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})}}
		_, err := c.Templates(t.Context(), 0)
		if calls != 3 || time.Since(start) != 25*time.Second || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
		}
	})
}

// A non-success status is authoritative even when its body cannot be read.
type unreadableDocuSealBody struct{ closed *bool }

func (b unreadableDocuSealBody) Read([]byte) (int, error) { panic("error body was read") }
func (b unreadableDocuSealBody) Close() error             { *b.closed = true; return nil }

func TestProviderErrorStatusDoesNotReadOrRetryBody(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, kind := range []string{"oversized", "unreadable"} {
			for _, operation := range []string{"check", "templates"} {
				t.Run(fmt.Sprintf("%d/%s/%s", status, kind, operation), func(t *testing.T) {
					calls := 0
					closed := false
					c := &Client{connection: config.DocuSealConnection{Endpoint: "https://example.test", APIKey: "synthetic-key"}, http: &http.Client{Transport: docusealRoundTripFunc(func(*http.Request) (*http.Response, error) {
						calls++
						var body io.ReadCloser = unreadableDocuSealBody{closed: &closed}
						if kind == "oversized" {
							body = &trackedDocuSealBody{Reader: strings.NewReader(strings.Repeat("x", maxResponse+1)), closed: &closed}
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
					})}}
					var err error
					if operation == "check" {
						_, err = c.Check(t.Context())
					} else {
						_, err = c.Templates(t.Context(), 0)
					}
					if err == nil || err.Error() != "docuseal_unauthorized" || calls != 1 || !closed {
						t.Fatalf("err=%v calls=%d closed=%v", err, calls, closed)
					}
				})
			}
		}
	}
}

type trackedDocuSealBody struct {
	io.Reader
	closed *bool
}

func (b *trackedDocuSealBody) Close() error { *b.closed = true; return nil }

func TestConnectionRefusesRedirect(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer target.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer provider.Close()
	c, err := New(config.DocuSealConnection{Endpoint: provider.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	_, err = c.Check(t.Context())
	if err == nil || err.Error() != "docuseal_provider_error" || reached {
		t.Fatalf("err=%v reached=%v", err, reached)
	}
}

func TestTemplatesEmptyNameUsesPlaceholder(t *testing.T) {
	c := &Client{connection: config.DocuSealConnection{Endpoint: "https://example.test", APIKey: "synthetic-key"}, http: &http.Client{Transport: docusealRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":1,"name":"","submitters":[]}]}`))}, nil
	})}}
	page, err := c.Templates(t.Context(), 0)
	if err != nil || len(page.Templates) != 1 || page.Templates[0].Name != "(unnamed)" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}
