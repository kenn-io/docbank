package jsontext

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	for name, raw := range map[string][]byte{
		"ordinary text":          []byte(`{"path":"/inbox/file.txt"}`),
		"raw Unicode":            []byte(`{"path":"/inbox/😀.txt"}`),
		"surrogate pair":         []byte(`{"path":"/inbox/\ud83d\ude00.txt"}`),
		"escaped surrogate text": []byte(`{"path":"/inbox/\\ud800.txt"}`),
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, Validate(raw, "payload"))
		})
	}

	for name, tt := range map[string]struct {
		raw  []byte
		want string
	}{
		"invalid UTF-8": {raw: []byte{'{', '"', 0xff, '"', '}'}, want: "payload is not valid UTF-8"},
		"lone high surrogate": {
			raw: []byte(`{"path":"\ud800"}`), want: "unpaired UTF-16 surrogate escape",
		},
		"lone low surrogate": {
			raw: []byte(`{"path":"\udc00"}`), want: "unpaired UTF-16 surrogate escape",
		},
		"high followed by high": {
			raw: []byte(`{"path":"\ud800\ud801"}`), want: "unpaired UTF-16 surrogate escape",
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, Validate(tt.raw, "payload"), tt.want)
		})
	}
}

func TestValidateValueRejectsNestedInvalidUTF8(t *testing.T) {
	invalid := string([]byte{'/', 'b', 'a', 'd', 0xff})
	value := map[string]any{"paths": []string{"/good", invalid}}
	require.ErrorContains(t, ValidateValue(value, "request"), "is not valid UTF-8")
	require.NoError(t, ValidateValue(map[string]any{"paths": []string{"/good"}}, "request"))
}
