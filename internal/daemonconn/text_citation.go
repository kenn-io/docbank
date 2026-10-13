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
	var transport struct {
		document.ResolvedTextCitation

		Schema string `json:"$schema,omitzero"`
	}
	if err := decodeCitationResponse(response, &transport, 1<<20); err != nil {
		return document.ResolvedTextCitation{}, err
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

func decodeCitationResponse(response *http.Response, output any, maxBytes int64) error {
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("reading retained text response: %w", err)
	}
	if int64(len(encoded)) > maxBytes {
		return errors.New("retained text response is too large")
	}
	if err := json.Unmarshal(encoded, output, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("decoding retained text response: %w", err)
	}
	return nil
}
