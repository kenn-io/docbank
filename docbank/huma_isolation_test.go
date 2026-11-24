package docbank_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"

	_ "go.kenn.io/docbank/docbank" // Exercise the effect of importing the embedded API.
)

// A program that embeds the library and serves its own Huma API keeps Huma's
// defaults: its error responses, error schema, and array schemas.
func TestEmbeddedImportPreservesHostHumaDefaults(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Example host", "1"))
	type listOutput struct {
		Body struct {
			Names []string `json:"names"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "list-names", Method: http.MethodGet, Path: "/example",
		Errors: []int{http.StatusBadRequest},
	}, func(context.Context, *struct{}) (*listOutput, error) {
		return nil, huma.Error400BadRequest("invalid selection", &huma.ErrorDetail{
			Location: "query.selection", Message: "select a document",
		})
	})

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/example", nil))
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Equal(t, "application/problem+json", response.Header().Get("Content-Type"))
	var problem huma.ErrorModel
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	require.Equal(t, "invalid selection", problem.Detail)
	require.Len(t, problem.Errors, 1)
	require.Equal(t, "query.selection", problem.Errors[0].Location)

	operation := api.OpenAPI().Paths["/example"].Get
	errorSchema := operation.Responses["400"].Content["application/problem+json"].Schema
	require.Equal(t, "#/components/schemas/ErrorModel", errorSchema.Ref)
	body := operation.Responses["200"].Content["application/json"].Schema
	output := api.OpenAPI().Components.Schemas.SchemaFromRef(body.Ref)
	require.True(t, output.Properties["names"].Nullable)
}
