package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestOperationPolicyRechecksPersistedSourceGrant(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	sourceID := "11111111-1111-4111-8111-111111111111"
	principal := Principal{SubjectID: "subject:synthetic", CredentialKind: "machine",
		Audience: "docbank:test", Operations: []Operation{OperationProcessing},
		SourceIDs: []string{sourceID}, GrantRevision: 7, ExpiresAt: now.Add(time.Hour)}
	authority := &testGrantAuthority{grant: principal}
	policy := NewOperationPolicy(OperationPolicyOptions{
		Authority: authority, Now: func() time.Time { return now },
	})
	binding := store.SourceGrantBinding{SubjectID: principal.SubjectID,
		CredentialKind: principal.CredentialKind, Audience: principal.Audience,
		GrantRevision: principal.GrantRevision, ExpiresAt: principal.ExpiresAt, SourceID: sourceID}
	if err := policy.AuthorizeSourceGrant(t.Context(), binding); err != nil {
		t.Fatalf("current grant was denied: %v", err)
	}
	authority.set(func(grant *Principal) { grant.GrantRevision++ })
	if err := policy.AuthorizeSourceGrant(t.Context(), binding); !errors.Is(err, ErrOperationGrantRevoked) {
		t.Fatalf("revoked grant result = %v", err)
	}
}

func TestDefaultServerOperationPolicyWritesSafeAuditRecords(t *testing.T) {
	var output bytes.Buffer
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	server := NewServer(Deps{
		Cfg:    cfg,
		Logger: slog.New(slog.NewJSONHandler(&output, nil)),
	})
	_, err := server.deps.OperationPolicy.Authorize(t.Context(), OperationAuthorizationRequest{
		Principal: LocalAdminPrincipal(), Operation: OperationRead,
		SourceIDs: []string{"synthetic-private-source"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode audit record: %v", err)
	}
	if record["msg"] != "operation_authorization" ||
		record["actor"] != "local:admin" ||
		record["operation"] != string(OperationRead) ||
		record["outcome"] != string(OperationOutcomeAllowed) {
		t.Fatalf("unexpected audit record: %#v", record)
	}
	if strings.Contains(output.String(), "synthetic-private-source") {
		t.Fatal("audit record exposed source identity")
	}
}

func TestOperationPolicyWithAuditIfAbsentPreservesConfiguredSink(t *testing.T) {
	configured := &testOperationAudit{}
	fallback := &testOperationAudit{}
	policy := NewOperationPolicy(OperationPolicyOptions{Audit: configured})
	if got := policy.WithAuditIfAbsent(fallback); got != policy {
		t.Fatal("configured policy was replaced")
	}
	_, err := policy.Authorize(t.Context(), OperationAuthorizationRequest{
		Principal: LocalAdminPrincipal(), Operation: OperationRead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(configured.events) != 1 || len(fallback.events) != 0 {
		t.Fatalf("configured events = %d, fallback events = %d", len(configured.events), len(fallback.events))
	}
}

func TestIntersectSourceIDs(t *testing.T) {
	got := IntersectSourceIDs([]string{"a", "secret", "a", "b"}, []string{"b", "a"})
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
	if len(IntersectSourceIDs([]string{"secret"}, nil)) != 0 {
		t.Fatal("scope widened")
	}
}

func TestOperationPolicyAuthorizesStableMixedScope(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	principal := testPrincipal(now)
	policy := NewOperationPolicy(OperationPolicyOptions{
		Authority: &testGrantAuthority{grant: principal},
		Now:       func() time.Time { return now },
	})

	first, err := policy.Authorize(t.Context(), OperationAuthorizationRequest{
		Principal: principal, Operation: OperationRead,
		SourceIDs: []string{"source-a", "hidden", "source-a", "source-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.SourceIDs, []string{"source-a", "source-b"}) {
		t.Fatalf("source fence = %v", first.SourceIDs)
	}
	second, err := policy.Authorize(t.Context(), OperationAuthorizationRequest{
		Principal: principal, Operation: OperationRead,
		SourceIDs: []string{"source-b", "source-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.CacheKey != second.CacheKey {
		t.Fatalf("cache key changed with source order: %q != %q", first.CacheKey, second.CacheKey)
	}
}

func TestOperationPolicyDeniesMissingOperationAndOversizeScope(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	principal := testPrincipal(now)
	policy := NewOperationPolicy(OperationPolicyOptions{
		Authority: &testGrantAuthority{grant: principal},
		Now:       func() time.Time { return now },
	})

	_, err := policy.Authorize(t.Context(), OperationAuthorizationRequest{
		Principal: principal, Operation: OperationContribute,
	})
	if !errors.Is(err, ErrOperationDenied) {
		t.Fatalf("missing operation error = %v", err)
	}

	overLimit := make([]string, MaxOperationSourceIDs+1)
	for i := range overLimit {
		overLimit[i] = time.Unix(int64(i), 0).UTC().Format(time.RFC3339Nano)
	}
	_, err = policy.Authorize(t.Context(), OperationAuthorizationRequest{
		Principal: principal, Operation: OperationRead, SourceIDs: overLimit,
	})
	if !errors.Is(err, ErrOperationScopeTooLarge) {
		t.Fatalf("oversize scope error = %v", err)
	}
}

func TestOperationPolicyRejectsExpiredAndRevokedGrantAfterCachedDecision(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	principal := testPrincipal(now)
	authority := &testGrantAuthority{grant: principal}
	policy := NewOperationPolicy(OperationPolicyOptions{
		Authority: authority, Now: func() time.Time { return now },
	})
	request := OperationAuthorizationRequest{
		Principal: principal, Operation: OperationRead, SourceIDs: []string{"source-a"},
	}
	if _, err := policy.Authorize(t.Context(), request); err != nil {
		t.Fatalf("fill cache decision: %v", err)
	}

	authority.set(func(grant *Principal) { grant.GrantRevision++ })
	if _, err := policy.Authorize(t.Context(), request); !errors.Is(err, ErrOperationGrantRevoked) {
		t.Fatalf("revoked grant error = %v", err)
	}

	expired := testPrincipal(now)
	expired.ExpiresAt = now
	authority.setGrant(expired)
	request.Principal = expired
	if _, err := policy.Authorize(t.Context(), request); !errors.Is(err, ErrOperationGrantExpired) {
		t.Fatalf("expired grant error = %v", err)
	}
}

func TestOperationPolicyHidesProtectedAndDerivedSelectors(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	principal := testPrincipal(now)
	policy := NewOperationPolicy(OperationPolicyOptions{
		Authority: &testGrantAuthority{grant: principal},
		Now:       func() time.Time { return now },
	})
	for _, request := range []OperationAuthorizationRequest{
		{Principal: principal, Operation: OperationRead, SourceIDs: []string{"hidden"}, RequireAll: true, Protected: true},
		{Principal: principal, Operation: OperationAnalyze, SourceIDs: []string{"source-a", "hidden"}, RequireAll: true, Protected: true},
	} {
		if _, err := policy.Authorize(t.Context(), request); !errors.Is(err, ErrOperationNotFound) {
			t.Fatalf("protected authorization error = %v", err)
		}
	}
}

func TestOperationPolicyAuditContainsOnlySafeFields(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	principal := testPrincipal(now)
	audit := &testOperationAudit{}
	policy := NewOperationPolicy(OperationPolicyOptions{
		Authority: &testGrantAuthority{grant: principal}, Audit: audit,
		Now: func() time.Time { return now },
	})
	_, err := policy.Authorize(t.Context(), OperationAuthorizationRequest{
		Principal: principal, Operation: OperationRead,
		SourceIDs: []string{"source-a", "not-authorized"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.events) != 1 {
		t.Fatalf("audit events = %d", len(audit.events))
	}
	event := audit.events[0]
	if event.Actor != principal.SubjectID || event.Operation != OperationRead ||
		event.Outcome != OperationOutcomeAllowed || event.GrantRevision != principal.GrantRevision ||
		event.SourceCount != 1 {
		t.Fatalf("audit event = %#v", event)
	}
	for _, field := range reflect.VisibleFields(reflect.TypeFor[OperationAuditEvent]()) {
		if slices.Contains([]string{"Token", "SourceIDs", "SourceText"}, field.Name) {
			t.Fatalf("audit event exposes unsafe field %s", field.Name)
		}
	}
}

func TestOperationPolicyKeepsLocalAdminCompatible(t *testing.T) {
	policy := NewOperationPolicy(OperationPolicyOptions{})
	decision, err := policy.Authorize(t.Context(), OperationAuthorizationRequest{
		Principal: LocalAdminPrincipal(), Operation: OperationMetadataMutation,
		SourceIDs: []string{"source-b", "source-a"}, RequireAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Unrestricted || !reflect.DeepEqual(decision.SourceIDs, []string{"source-b", "source-a"}) {
		t.Fatalf("local admin decision = %#v", decision)
	}
}

func testPrincipal(now time.Time) Principal {
	return Principal{
		SubjectID: "subject:synthetic", CredentialKind: "machine", Audience: "docbank:test",
		Operations: []Operation{OperationRead, OperationAnalyze},
		SourceIDs:  []string{"source-b", "source-a"}, GrantRevision: 7,
		ExpiresAt: now.Add(time.Hour),
	}
}

type testGrantAuthority struct {
	mu    sync.Mutex
	grant Principal
}

func (a *testGrantAuthority) CurrentGrant(_ context.Context, _ string) (Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.grant, nil
}

func (a *testGrantAuthority) set(mutate func(*Principal)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	mutate(&a.grant)
}

func (a *testGrantAuthority) setGrant(grant Principal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grant = grant
}

type testOperationAudit struct{ events []OperationAuditEvent }

func (a *testOperationAudit) RecordOperation(_ context.Context, event OperationAuditEvent) {
	a.events = append(a.events, event)
}
