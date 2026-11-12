package docusealclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/config"
)

func TestConnectionChecksAndListsTemplates(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != "synthetic-key" {
			t.Error("missing credential")
		}
		switch r.URL.Path {
		case "/api/user":
			fmt.Fprint(w, `{"email":"private@example.test"}`)
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
	if err != nil || len(page.Templates) != 1 || page.Templates[0].Roles[0] != "Tenant" {
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
