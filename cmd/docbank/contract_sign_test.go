package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/daemonconn"
)

func TestContractSignDaemonWalk(t *testing.T) {
	configured := true
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !configured {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"status":503,"title":"Unconfigured","detail":"Enable DocuSeal","code":"docuseal_unconfigured"}`)
			return
		}
		switch r.URL.Path {
		case "/api/v1/integrations/docuseal/status":
			fmt.Fprint(w, `{"configured":true,"enabled":true,"endpoint":"https://example.test","capability":"template_submission"}`)
		case "/api/v1/integrations/docuseal/templates":
			if r.URL.Query().Get("after") != "42" {
				t.Errorf("cursor: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"templates":[{"id":7,"name":"Lease","roles":["Buyer, Seller","Buyer","Seller"]}],"next":0}`)
		default:
			http.NotFound(w, r)
		}
	})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"contract", "sign", "check"}, "Connected to https://example.test"},
		{[]string{"contract", "sign", "check", "--json"}, `"capability"`},
		{[]string{"contract", "sign", "templates", "--after", "42"}, `"Buyer, Seller", "Buyer", "Seller"`},
		{[]string{"contract", "sign", "templates", "--after", "42", "--json"}, `"roles"`},
	} {
		out, err := runCLI(t, tc.args...)
		if err != nil || !strings.Contains(out, tc.want) {
			t.Fatalf("%v: %q: %v", tc.args, out, err)
		}
	}
	configured = false
	_, err := runCLI(t, "contract", "sign", "check")
	code, _ := daemonconn.ProblemCode(err)
	if err == nil || code != "docuseal_unconfigured" {
		t.Fatalf("unconfigured: %v", err)
	}
}
