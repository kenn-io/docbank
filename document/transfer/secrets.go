package transfer

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"

	"encoding/json/jsontext"
)

var (
	errForbiddenStructuredKey = errors.New("transfer: forbidden structured key")
	errStructuredDepth        = errors.New("transfer: structured JSON depth limit")
	errInvalidStructuredJSON  = errors.New("transfer: invalid structured JSON")
)

var (
	forbiddenObjectPrefixes = []string{
		"credential", "token", "secret", "oauth", "cursor", "sync", "carddav",
		"imap", "session", "cookie", "enrichment",
	}
	forbiddenObjectNames = []string{
		"password", "passphrase", "api_key", "apikey", "access_token", "refresh_token",
		"client_secret", "uidvalidity", "provider_credential", "private_notes",
	}
)

func ForbiddenObjectKey(key string) bool {
	lower := strings.ToLower(key)
	for _, prefix := range forbiddenObjectPrefixes {
		if lower == prefix || strings.HasPrefix(lower, prefix+".") {
			return true
		}
	}
	return slices.Contains(forbiddenObjectNames, lower)
}

func CheckStructuredKeys(raw jsontext.Value) error {
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	if err := scanStructuredValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.ReadToken(); !errors.Is(err, io.EOF) {
		return errInvalidStructuredJSON
	}
	return nil
}

func scanStructuredValue(decoder *jsontext.Decoder, depth int) error {
	switch decoder.PeekKind() {
	case jsontext.KindBeginObject:
		if depth >= MaxJSONDepth {
			return errStructuredDepth
		}
		if _, err := decoder.ReadToken(); err != nil {
			return errInvalidStructuredJSON
		}
		for decoder.PeekKind() != jsontext.KindEndObject {
			key, err := decoder.ReadToken()
			if err != nil || key.Kind() != jsontext.KindString {
				return errInvalidStructuredJSON
			}
			if ForbiddenObjectKey(key.String()) {
				return errForbiddenStructuredKey
			}
			if err := scanStructuredValue(decoder, depth+1); err != nil {
				return err
			}
		}
		if _, err := decoder.ReadToken(); err != nil {
			return errInvalidStructuredJSON
		}
	case jsontext.KindBeginArray:
		if depth >= MaxJSONDepth {
			return errStructuredDepth
		}
		if _, err := decoder.ReadToken(); err != nil {
			return errInvalidStructuredJSON
		}
		for decoder.PeekKind() != jsontext.KindEndArray {
			if err := scanStructuredValue(decoder, depth+1); err != nil {
				return err
			}
		}
		if _, err := decoder.ReadToken(); err != nil {
			return errInvalidStructuredJSON
		}
	case jsontext.KindInvalid, jsontext.KindEndObject, jsontext.KindEndArray:
		return errInvalidStructuredJSON
	default:
		if _, err := decoder.ReadToken(); err != nil {
			return errInvalidStructuredJSON
		}
	}
	return nil
}
