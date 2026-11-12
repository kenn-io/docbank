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
	"net/url"
	"strconv"
	"strings"
	"testing"

	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/config"
)

func TestConnectionChecksAndListsTemplates(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != "synthetic-key" {
			t.Error("missing credential")
		}
		switch r.URL.Path {
		case "/api/user":
			fmt.Fprint(w, `{"id":7,"email":"private@example.test"}`)
		case "/api/templates":
			fmt.Fprint(w, `{"data":[{"id":12,"name":"Lease","submitters":[{"name":"Tenant"}]}],"pagination":{"next":12}}`)
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
func TestConnectionRejectsRedirectAndSecretEcho(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			http.Redirect(w, r, "/elsewhere", 302)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":1,"name":"synthetic-key"}]}`)
	}))
	defer s.Close()
	c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Check(t.Context()); err == nil {
		t.Fatal("accepted redirect")
	}
	_, err = c.Templates(t.Context(), 0)
	if err == nil || strings.Contains(err.Error(), "synthetic-key") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestConnectionRejectsOversizedResponse(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxResponse+1)))
	}))
	defer s.Close()
	c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Check(t.Context())
	if err == nil || err.Error() != "docuseal_response_too_large" {
		t.Fatalf("got %v", err)
	}
}

func TestConnectionRejectsMalformedProviderShapes(t *testing.T) {
	for _, body := range []string{"null", "{}", `{"id":0}`} {
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
	for _, body := range []string{"null", "{}", `{"data":null}`} {
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

func TestConnectionRejectsCompressedOversizeAndRedactsErrors(t *testing.T) {
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
			if _, err = c.Check(t.Context()); err == nil || err.Error() != "docuseal_response_too_large" {
				t.Fatalf("got %v", err)
			}
		})
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic-key https://example.test/private", http.StatusBadRequest)
	}))
	defer s.Close()
	c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Check(t.Context())
	if err == nil || strings.Contains(err.Error(), "synthetic-key") || strings.Contains(err.Error(), "example.test") {
		t.Fatalf("unsafe error: %v", err)
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
				fmt.Fprintf(w, `{"data":[%s],"pagination":{"next":%d}}`, strings.Join(items, ","), 200-count)
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

type fixedResolver struct{ addresses []netip.Addr }

func (r fixedResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	return r.addresses, nil
}

func TestConnectionRejectsMixedDNSAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("denied DNS answer reached server") }))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	prefix := netip.MustParsePrefix("127.0.0.0/8")
	c, err := New(config.DocuSealConnection{Endpoint: "http://docuseal.invalid:" + u.Port(), APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{prefix}})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := providerhttp.NewTransport(providerhttp.EgressPolicy{Scheme: "http", Host: "docuseal.invalid", Port: uint16(port), AllowedCIDRs: []netip.Prefix{prefix}}, fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.0.2.1")}})
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = transport
	if _, err = c.Check(t.Context()); err == nil {
		t.Fatal("accepted mixed DNS answers")
	}
}

func TestTemplatesReducesOversizedPages(t *testing.T) {
	var requests []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RawQuery)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		after := r.URL.Query().Get("after")
		if after == "51" {
			fmt.Fprint(w, `{"data":[{"id":50,"name":"Last"}],"pagination":{"next":50}}`)
			return
		}
		if limit > 50 {
			_, _ = io.WriteString(w, strings.Repeat("x", maxResponse+1))
			return
		}
		fmt.Fprint(w, `{"data":[`)
		for i := range limit {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"id":%d,"name":"Template","fields":"%s"}`, 100-i, strings.Repeat("x", 1000))
		}
		fmt.Fprint(w, `],"pagination":{"next":51}}`)
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
}

func TestTemplatesIrreducibleOversize(t *testing.T) {
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
}

type docusealRoundTripFunc func(*http.Request) (*http.Response, error)

func (f docusealRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type docusealErrorBody struct{ err error }

func (b docusealErrorBody) Read([]byte) (int, error) { return 0, b.err }
func (b docusealErrorBody) Close() error             { return nil }

func TestCheckPreservesBodyContextErrors(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		c := &Client{connection: config.DocuSealConnection{Endpoint: "https://example.test", APIKey: "synthetic-key"}, http: &http.Client{Transport: docusealRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: docusealErrorBody{err: want}}, nil
		})}}
		_, err := c.Check(t.Context())
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	}
}

func TestCheckRedactsKeyInEndpoint(t *testing.T) {
	c := &Client{connection: config.DocuSealConnection{Endpoint: "https://key.example.test", APIKey: "key"}, http: &http.Client{Transport: docusealRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":1}`))}, nil
	})}}
	status, err := c.Check(t.Context())
	if err != nil || strings.Contains(status.Endpoint, "key") || status.Endpoint != "[redacted]" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
