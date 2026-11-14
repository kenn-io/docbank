//go:build !darwin || arm64

package kataclient

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"go.kenn.io/docbank/internal/config"
	"hegel.dev/go/hegel"
)

// Hegel's native engine supports macOS arm64, not macOS amd64. The service
// regressions in client_test.go exercise admission on every supported platform.
func TestSupportedSchemaAcceptsCompatibleMinors(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		minor := hegel.Draw(ht, hegel.Integers(21, math.MaxInt))
		patch := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		version := fmt.Sprintf("0.%d.%d", minor, patch)
		if !supportedSchema(version) {
			ht.Fatalf("compatible schema %q was rejected", version)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestSupportedSchemaRejectsBelowMinimum(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		minor := hegel.Draw(ht, hegel.Integers(0, 20))
		patch := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		version := fmt.Sprintf("0.%d.%d", minor, patch)
		if supportedSchema(version) {
			ht.Fatalf("schema %q below the minimum was accepted", version)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestSupportedSchemaRejectsNewerMajors(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		major := hegel.Draw(ht, hegel.Integers(1, math.MaxInt))
		minor := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		patch := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		version := fmt.Sprintf("%d.%d.%d", major, minor, patch)
		if supportedSchema(version) {
			ht.Fatalf("newer major schema %q was accepted", version)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestSupportedSchemaPrefixAndMetadataPreserveAdmission(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		major := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		minor := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		patch := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		version := fmt.Sprintf("%d.%d.%d", major, minor, patch)
		build := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		if supportedSchema(fmt.Sprintf("v%s+build-%d", version, build)) != supportedSchema(version) {
			ht.Fatalf("prefix and build metadata changed admission of %q", version)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestSupportedSchemaAcceptsLaterMinorPrereleases(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		minor := hegel.Draw(ht, hegel.Integers(22, math.MaxInt))
		patch := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		rc := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		version := fmt.Sprintf("0.%d.%d-rc.%d", minor, patch, rc)
		if !supportedSchema(version) {
			ht.Fatalf("prerelease %q above the floor was rejected", version)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestSupportedSchemaAcceptsLaterPatchPrereleases(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		patch := hegel.Draw(ht, hegel.Integers(1, math.MaxInt))
		rc := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		version := fmt.Sprintf("0.21.%d-rc.%d", patch, rc)
		if !supportedSchema(version) {
			ht.Fatalf("prerelease %q above the floor was rejected", version)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestSupportedSchemaRejectsMalformedNumericCores(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		minor := hegel.Draw(ht, hegel.Integers(21, math.MaxInt))
		patch := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		parts := []string{"0", strconv.Itoa(minor), strconv.Itoa(patch)}
		component := hegel.Draw(ht, hegel.Integers(0, 2))
		malformation := hegel.Draw(ht, hegel.SampledFrom([]string{"", "+1", "-1", " 1", "１", "18446744073709551616", "01"}))
		parts[component] = malformation
		version := fmt.Sprintf("%s.%s.%s", parts[0], parts[1], parts[2])
		if supportedSchema(version) {
			ht.Fatalf("malformed numeric core %q was accepted", version)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestClientSchemaGateCoversEveryRemoteOperation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		minor := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		version := fmt.Sprintf("1.%d.0-rc.1", minor)
		var operationRequests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/health" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"api_schema_version":%q}`, version)
				return
			}
			operationRequests.Add(1)
			http.Error(w, "unexpected operation", http.StatusInternalServerError)
		}))
		defer server.Close()
		for _, operation := range []string{"get", "create", "patch", "comment", "comments", "marker"} {
			c := New(config.KataConnection{Endpoint: server.URL, APIKey: "synthetic-key", Project: "docbank"})
			err := schemaOperation(t.Context(), c, operation)
			if err == nil || err.Error() != "unsupported Kata API schema: require >=0.21.0, <1.0.0" || operationRequests.Load() != 0 {
				ht.Fatalf("%s bypassed schema admission for %q: requests=%d, err=%v", operation, version, operationRequests.Load(), err)
			}
		}
	}, hegel.WithDatabase(t.TempDir()))
}
