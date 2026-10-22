package api_test

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

// hiddenScopeContext schedules a real lock at the next visibility read after
// the cursor binding. Context has no standard hook for coordinating Value reads.
type hiddenScopeContext struct {
	context.Context

	token string
	reads int
	lock  func()
}

func (ctx *hiddenScopeContext) Value(key any) any {
	value := ctx.Context.Value(key)
	if token, ok := value.(string); ok && token == ctx.token {
		ctx.reads++
		if ctx.reads == 2 {
			ctx.lock()
		}
	}
	return value
}

func TestPeopleCursorScopeMatchesPageDuringLock(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	for _, name := range []string{"Ada", "Grace"} {
		person, err := s.CreatePerson(ctx, name, "operator")
		require.NoError(t, err)
		hash, size, err := s.Blobs.Write(strings.NewReader("synthetic photo of " + name))
		require.NoError(t, err)
		node, err := s.CreateFile(ctx, s.RootID(), name+".jpg", hash, size, "image/jpeg")
		require.NoError(t, err)
		_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
		require.NoError(t, err)
	}
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	requestCtx := &hiddenScopeContext{
		Context: store.WithPhotoHiddenToken(ctx, token), token: token,
		lock: func() { require.NoError(t, s.LockPhotoHidden(ctx)) },
	}
	request := httptest.NewRequestWithContext(requestCtx, http.MethodGet, ts.URL+"/api/v1/people?limit=1&photo_summary=true", nil)
	request.Header.Set("X-Api-Key", testAPIKey)
	response := httptest.NewRecorder()
	s.Server.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var page api.PersonPage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.Items[0].PhotoSummary)
	cursor, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	require.NoError(t, err)
	parts := strings.SplitN(string(cursor), "\x00", 4)
	require.Len(t, parts, 4)
	binding := strings.Split(parts[3], "\x01")
	require.Len(t, binding, 6)
	require.Equal(t, page.Items[0].PhotoSummary.HiddenCount != nil, binding[4] == "true", "cursor unlock scope must match the page's Hidden projection")
}
