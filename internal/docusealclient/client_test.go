package docusealclient

import (
	"compress/gzip"
	"context"
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
	if err != nil || len(page.Templates) != 1 || len(page.Templates[0].Roles) != 1 || page.Templates[0].Roles[0] != "Tenant" || page.Next != 12 {
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
	for _, body := range []string{`{"data":[{"id":1,"name":"bad\u001btext"}]}`, `{"data":[{"id":1,"name":"ok","submitters":[{"name":"synthetic-key"}]}]}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("after") != "12" {
					t.Error("missing cursor")
				}
				_, _ = io.WriteString(w, body)
			}))
			defer s.Close()
			c, err := New(config.DocuSealConnection{Endpoint: s.URL, APIKey: "synthetic-key", AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Templates(t.Context(), 12); err == nil {
				t.Fatal("accepted unsafe text")
			}
		})
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
