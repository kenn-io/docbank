package embedconfig

import (
	"errors"
	"strings"
)

func (m Model) normalize() Model {
	m.Name = strings.TrimSpace(m.Name)
	m.Revision = strings.TrimSpace(m.Revision)
	m.Pooling = strings.TrimSpace(m.Pooling)
	m.EncodingFormat = strings.TrimSpace(m.EncodingFormat)
	m.Metric = Metric(strings.TrimSpace(string(m.Metric)))
	m.Normalization = Normalization(strings.TrimSpace(string(m.Normalization)))
	return m
}

// Prepared trims the model fields and validates them.
func (m Model) Prepared() (Model, error) {
	m = m.normalize()
	return m, m.Validate()
}

// Validate checks the model controls that define a vector space.
// Only cosine is storable. Dot-product and L2 stay named on Metric and fail
// here because the vector pipeline compares cosine distance only.
func (m Model) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("embed model name is required")
	}
	if m.Dimensions <= 0 {
		return errors.New("embed model dimensions must be positive")
	}
	if m.Metric != MetricCosine {
		return errors.New("embed model metric must be cosine; dot_product and l2 are not storable yet")
	}
	switch m.Normalization {
	case NormalizationNone, NormalizationL2:
	default:
		return errors.New("embed model normalization must be none or l2")
	}
	return nil
}

func (r Roles) normalize() Roles {
	r.DocumentFormatter = strings.TrimSpace(r.DocumentFormatter)
	r.QueryFormatter = strings.TrimSpace(r.QueryFormatter)
	if r.InputType == "" {
		r.InputType = InputTypeNone
	}
	return r
}

// Prepared fills an empty input type with none and validates the roles.
func (r Roles) Prepared() (Roles, error) {
	r = r.normalize()
	return r, r.Validate()
}

// Validate checks role labels and the input type mode.
func (r Roles) Validate() error {
	switch r.normalizedType() {
	case InputTypeNone, InputTypeRetrieval:
	default:
		return errors.New("embed input type must be none or retrieval")
	}
	return nil
}

func (r Roles) normalizedType() InputType {
	if r.InputType == "" {
		return InputTypeNone
	}
	return r.InputType
}

func (d Deployment) normalize() Deployment {
	d.BaseURL = strings.TrimSpace(d.BaseURL)
	return d
}

// Prepared trims the endpoint and validates it.
func (d Deployment) Prepared() (Deployment, error) {
	d = d.normalize()
	return d, d.Validate()
}

// Validate checks the endpoint and resolves its canonical form.
func (d Deployment) Validate() error {
	if strings.TrimSpace(d.BaseURL) == "" {
		return errors.New("embed endpoint is required")
	}
	_, err := d.Canonical()
	return err
}

// Canonical returns the pinned form of the endpoint.
func (d Deployment) Canonical() (string, error) {
	return CanonicalEndpoint(d.BaseURL, d.TrustPrivateNetwork)
}

// Prepared fills a zero item count and validates the batch.
func (b Batch) Prepared() (Batch, error) {
	b = b.ApplyDefaults()
	return b, b.Validate()
}

// ApplyDefaults fills a zero item count. It does not change token limits.
func (b Batch) ApplyDefaults() Batch {
	if b.Items == 0 {
		b.Items = DefaultBatchItems
	}
	return b
}

// Validate checks the item cap and the optional token budget.
func (b Batch) Validate() error {
	if b.Items <= 0 {
		return errors.New("embed batch items must be positive")
	}
	tokenSet := b.MaxTokens != 0 || b.InputTokenUpperBound != 0
	if !tokenSet {
		return nil
	}
	if b.MaxTokens <= 0 || b.InputTokenUpperBound <= 0 {
		return errors.New("embed batch token budget needs a positive aggregate cap and per-input bound")
	}
	if b.InputTokenUpperBound > b.MaxTokens {
		return errors.New("embed batch per-input token bound must fit in the aggregate cap")
	}
	return nil
}

// Prepared fills a zero timeout and response cap, then validates them.
func (t Transport) Prepared() (Transport, error) {
	t = t.ApplyDefaults()
	return t, t.Validate()
}

// ApplyDefaults fills a zero timeout and a zero response cap.
func (t Transport) ApplyDefaults() Transport {
	if t.Timeout == 0 {
		t.Timeout = DefaultTimeout
	}
	if t.MaxResponseBytes == 0 {
		t.MaxResponseBytes = DefaultMaxResponseBytes
	}
	return t
}

// Validate checks the transport limits.
func (t Transport) Validate() error {
	if t.Timeout <= 0 {
		return errors.New("embed transport timeout must be positive")
	}
	if t.MaxResponseBytes <= 0 {
		return errors.New("embed transport response cap must be positive")
	}
	return nil
}

func (in InputLimits) normalize() InputLimits {
	in.Recipe = strings.TrimSpace(in.Recipe)
	in.Tokenizer = strings.TrimSpace(in.Tokenizer)
	in.TokenizerRevision = strings.TrimSpace(in.TokenizerRevision)
	in.ContentID = strings.TrimSpace(in.ContentID)
	in.Truncation = Truncation(strings.TrimSpace(string(in.Truncation)))
	return in
}

// Prepared trims recipe labels and validates the input window.
func (in InputLimits) Prepared() (InputLimits, error) {
	in = in.normalize()
	return in, in.Validate()
}

// Validate allows an unset window. A token window needs a positive max, an
// explicit truncation policy, a tokenizer name, and an overlap below the max.
func (in InputLimits) Validate() error {
	window := in.MaxTokens != 0 || in.OverlapTokens != 0 || in.MaxSpans != 0 || in.Truncation != ""
	if !window {
		return nil
	}
	if in.MaxTokens <= 0 {
		return errors.New("embed input max tokens must be positive when a token window is set")
	}
	if in.OverlapTokens < 0 || in.OverlapTokens >= in.MaxTokens {
		return errors.New("embed input overlap must be at least zero and below max tokens")
	}
	// MaxSpans zero is unlimited, not unset, and stays valid inside a window.
	if in.MaxSpans < 0 {
		return errors.New("embed input max spans must be at least zero")
	}
	switch in.Truncation {
	case TruncationReject, TruncationDropTail:
	default:
		return errors.New("embed input truncation must be reject or drop_tail")
	}
	if in.Tokenizer == "" {
		return errors.New("embed input tokenizer is required when a token window is set")
	}
	return nil
}
