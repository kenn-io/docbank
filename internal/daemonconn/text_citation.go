package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/apiclient"
)

// ResolveTextCitation verifies that the response binds the exact requested quotation.
func (c *Connection) ResolveTextCitation(
	ctx context.Context, citation document.TextCitation,
) (document.ResolvedTextCitation, error) {
	if err := document.ValidateTextCitation(citation); err != nil {
		return document.ResolvedTextCitation{}, err
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).ResolveTextCitation(runtime.WithStreamingResponse(ctx),
		&apiclient.ResolveTextCitationRequestOptions{Body: &citation})
	if err != nil {
		return document.ResolvedTextCitation{}, err
	}
	defer func() { _ = response.Body.Close() }()
	const maxResponseBytes = 1 << 20
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return document.ResolvedTextCitation{}, fmt.Errorf("reading text citation response: %w", err)
	}
	if len(encoded) > maxResponseBytes {
		return document.ResolvedTextCitation{}, errors.New("text citation response is too large")
	}
	var transport struct {
		document.ResolvedTextCitation

		Schema string `json:"$schema,omitzero"`
	}
	if err := json.Unmarshal(encoded, &transport, json.RejectUnknownMembers(true)); err != nil {
		return document.ResolvedTextCitation{}, fmt.Errorf("decoding text citation response: %w", err)
	}
	result := transport.ResolvedTextCitation
	digest := sha256.Sum256([]byte(result.Text))
	if result.Citation != citation || !utf8.ValidString(result.Text) ||
		utf8.RuneCountInString(result.Text) != citation.End-citation.Start ||
		result.TextBytes != len(result.Text) || result.TextSHA256 != hex.EncodeToString(digest[:]) {
		return document.ResolvedTextCitation{},
			errors.New("text citation response does not match its quotation")
	}
	return result, nil
}
