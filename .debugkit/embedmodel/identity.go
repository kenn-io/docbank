package embedmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kit/embedconfig"
)

// vectorIdentity identifies a comparable vector space. The endpoint is
// included only when PinEndpoint is set. Batch, transport, and the wire
// encoding format are not inputs. Float and base64 are the same numbers.
func vectorIdentity(m embedconfig.Model, r embedconfig.Roles, d embedconfig.Deployment) (string, error) {
	fields, err := vectorFields(m, r, d)
	if err != nil {
		return "", err
	}
	return hashFields(fields), nil
}

// inputIdentity adds the recipe, tokenizer, content selection, and token
// window to the vector-space fields, because a model or role change also
// requires the inputs to be rebuilt. Operational limits are not included.
func inputIdentity(m embedconfig.Model, r embedconfig.Roles, d embedconfig.Deployment, in embedconfig.InputLimits) (string, error) {
	fields, err := vectorFields(m, r, d)
	if err != nil {
		return "", err
	}
	in, err = in.Prepared()
	if err != nil {
		return "", err
	}
	add(fields, "recipe", in.Recipe)
	add(fields, "tokenizer", in.Tokenizer)
	add(fields, "tokenizer_revision", in.TokenizerRevision)
	add(fields, "content", in.ContentID)
	if in.MaxTokens > 0 {
		fields["max_tokens"] = strconv.Itoa(in.MaxTokens)
		fields["overlap_tokens"] = strconv.Itoa(in.OverlapTokens)
		fields["max_spans"] = strconv.Itoa(in.MaxSpans)
		fields["truncation"] = string(in.Truncation)
	}
	return hashFields(fields), nil
}

func vectorFields(m embedconfig.Model, r embedconfig.Roles, d embedconfig.Deployment) (map[string]string, error) {
	m, r, d, err := prepareSpace(m, r, d)
	if err != nil {
		return nil, err
	}
	endpoint := ""
	if d.PinEndpoint {
		canonical, err := d.Canonical()
		if err != nil {
			return nil, err
		}
		endpoint = canonical
	}
	fields := map[string]string{
		"model":         m.Name,
		"dimensions":    strconv.Itoa(m.Dimensions),
		"metric":        string(m.Metric),
		"normalization": string(m.Normalization),
		"input_type":    string(r.InputType),
	}
	add(fields, "revision", m.Revision)
	add(fields, "pooling", m.Pooling)
	if m.RequestDimensions {
		fields["request_dimensions"] = "1"
	}
	add(fields, "document_formatter", r.DocumentFormatter)
	add(fields, "query_formatter", r.QueryFormatter)
	add(fields, "document_prefix", r.DocumentPrefix)
	add(fields, "document_suffix", r.DocumentSuffix)
	add(fields, "query_prefix", r.QueryPrefix)
	add(fields, "query_suffix", r.QuerySuffix)
	add(fields, "endpoint", endpoint)
	return fields, nil
}

func add(fields map[string]string, key, value string) {
	if value == "" {
		return
	}
	fields[key] = value
}

func hashFields(fields map[string]string) string {
	keys := slices.Sorted(maps.Keys(fields))
	var b strings.Builder
	b.WriteString("v1\n")
	for _, key := range keys {
		b.WriteString(escape(key))
		b.WriteByte('=')
		b.WriteString(escape(fields[key]))
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func escape(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, "\n", `\n`)
}

// prepareSpace trims and validates the parts that define a vector space. An
// empty deployment is allowed when the caller has not pinned an endpoint.
func prepareSpace(m embedconfig.Model, r embedconfig.Roles, d embedconfig.Deployment) (embedconfig.Model, embedconfig.Roles, embedconfig.Deployment, error) {
	m, err := m.Prepared()
	if err != nil {
		return m, r, d, err
	}
	r, err = r.Prepared()
	if err != nil {
		return m, r, d, err
	}
	d.BaseURL = strings.TrimSpace(d.BaseURL)
	if d.PinEndpoint || d.BaseURL != "" {
		if d, err = d.Prepared(); err != nil {
			return m, r, d, err
		}
	}
	return m, r, d, nil
}
