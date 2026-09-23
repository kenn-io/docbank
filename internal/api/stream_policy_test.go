package api

import (
	"bytes"
	"crypto/sha256"
	"io"
	"strings"
	"testing"
	"time"
)

func TestScopedStreamDoesNotWriteBytesAfterGrantRevocation(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	principal := testPrincipal(now)
	authority := &testGrantAuthority{grant: principal}
	policy := NewOperationPolicy(OperationPolicyOptions{
		Authority: authority, Now: func() time.Time { return now },
	})
	ctx := ContextWithPrincipal(t.Context(), principal)
	reader := &revokeBeforeRead{source: strings.NewReader("private bytes"), revoke: func() {
		authority.set(func(grant *Principal) { grant.GrantRevision++ })
	}}
	var output bytes.Buffer
	err := copyAuthorizedStream(ctx, Deps{OperationPolicy: policy}, "source-a", &output, reader, sha256.New())
	if err == nil || output.Len() != 0 {
		t.Fatalf("revoked stream wrote %q with error %v", output.String(), err)
	}
}

func TestScopedStreamWritesCurrentGrantBytes(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	principal := testPrincipal(now)
	policy := NewOperationPolicy(OperationPolicyOptions{
		Authority: &testGrantAuthority{grant: principal}, Now: func() time.Time { return now },
	})
	var output bytes.Buffer
	err := copyAuthorizedStream(ContextWithPrincipal(t.Context(), principal),
		Deps{OperationPolicy: policy}, "source-a", &output, strings.NewReader("synthetic bytes"), sha256.New())
	if err != nil || output.String() != "synthetic bytes" {
		t.Fatalf("current stream wrote %q with error %v", output.String(), err)
	}
}

type revokeBeforeRead struct {
	source io.Reader
	revoke func()
}

func (r *revokeBeforeRead) Read(p []byte) (int, error) {
	if r.revoke != nil {
		r.revoke()
		r.revoke = nil
	}
	return r.source.Read(p)
}
