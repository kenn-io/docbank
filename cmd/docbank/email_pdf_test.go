package main

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestEmailPDFCLIRejectsUnknownPaperBeforeDaemon(t *testing.T) {
	_, err := runCLI(t, "email-pdf", "12345678-1234-4234-8234-123456789abc", "unused.pdf", "--paper", "A3")
	require.ErrorContains(t, err, "A4 or Letter")
}
