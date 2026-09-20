package api

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMailboxBrowserBytesRequireVerifiedSocket(t *testing.T) {
	require.False(t, mailboxBrowserRequestAllowed(httptest.NewRequest(http.MethodPut, "/api/v1/mailbox/containers/source/chunks/0", nil)))
	require.False(t, mailboxBrowserRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/mailbox/transfers", nil)))
	require.True(t, mailboxBrowserRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/mailbox/containers/source/preview", nil)))
}
