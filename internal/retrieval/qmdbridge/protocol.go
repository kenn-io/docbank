package qmdbridge

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"mime"
	"strings"
	"unicode/utf8"
)

const (
	maximumFileBytes    = 2048
	maximumTitleBytes   = 4096
	maximumContextBytes = 4096
)

type wireRequest struct {
	Searches    []Search `json:"searches"`
	Limit       int      `json:"limit"`
	MinScore    float64  `json:"minScore"`
	Collections []string `json:"collections"`
	Intent      string   `json:"intent,omitempty"`
	Rerank      *bool    `json:"rerank,omitempty"`
}

type wireResponse struct {
	Results []wireResult `json:"results"`
}

type wireResult struct {
	DocID   string   `json:"docid"`
	File    string   `json:"file"`
	Title   string   `json:"title"`
	Score   *float64 `json:"score"`
	Context *string  `json:"context"`
	Snippet string   `json:"snippet"`
}

func decodeResponse(body []byte, limit int) ([]wireResult, error) {
	if limit < 0 || !utf8.Valid(body) {
		return nil, ErrInvalidResponse
	}
	unmarshalResults := json.UnmarshalFromFunc(func(decoder *jsontext.Decoder, target *[]wireResult) error {
		opening, err := decoder.ReadToken()
		if err != nil || opening.Kind() != '[' {
			return ErrInvalidResponse
		}
		items := make([]wireResult, 0, min(limit, 128))
		for decoder.PeekKind() != ']' {
			if len(items) == limit {
				return ErrInvalidResponse
			}
			var item wireResult
			if err := json.UnmarshalDecode(decoder, &item, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			items = append(items, item)
		}
		if _, err := decoder.ReadToken(); err != nil {
			return err
		}
		*target = items
		return nil
	})
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true), json.WithUnmarshalers(unmarshalResults)); err != nil || decoded.Results == nil {
		return nil, ErrInvalidResponse
	}
	for _, item := range decoded.Results {
		if !validWireResult(item, maximumSnippetBytes) {
			return nil, ErrInvalidResponse
		}
	}
	return decoded.Results, nil
}

func validWireResult(item wireResult, maxSnippet int) bool {
	if !validToken(item.DocID) || item.File == "" || len(item.File) > maximumFileBytes || !utf8.ValidString(item.File) ||
		len(item.Title) > maximumTitleBytes || !utf8.ValidString(item.Title) || len(item.Snippet) > maxSnippet ||
		!utf8.ValidString(item.Snippet) || item.Score == nil || !finiteUnit(*item.Score) {
		return false
	}
	return item.Context == nil || len(*item.Context) <= maximumContextBytes && utf8.ValidString(*item.Context)
}

func jsonContentType(value string) bool {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/json" {
		return false
	}
	charset, ok := parameters["charset"]
	return len(parameters) == 0 || len(parameters) == 1 && ok && strings.EqualFold(charset, "utf-8")
}
