package main

import (
	"bytes"
	"io"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestHumanAuditOutputQuotesPaths(t *testing.T) {
	const unsafePath = "/Taxes/\n\x1b[31mFORGED"
	want := strconv.QuoteToASCII(unsafePath)
	tests := []struct {
		name  string
		write func(io.Writer) error
	}{
		{
			name: "preview",
			write: func(w io.Writer) error {
				return writeAuditPreview(w, api.AuditEnrollmentPreview{TargetPath: unsafePath})
			},
		},
		{
			name: "enabled",
			write: func(w io.Writer) error {
				return writeAuditEnabled(w, api.AuditStatus{Scopes: []api.AuditScopeStatus{{
					TargetPath: unsafePath,
				}}})
			},
		},
		{
			name: "status",
			write: func(w io.Writer) error {
				return writeAuditStatus(w, api.AuditStatus{
					Enabled: true,
					Scopes:  []api.AuditScopeStatus{{TargetPath: unsafePath}},
					Membership: &api.AuditMembershipStatus{
						Path: unsafePath,
					},
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			require.NoError(t, test.write(&output))
			assert.Contains(t, output.String(), want)
			assert.NotContains(t, output.String(), unsafePath)
		})
	}
}
