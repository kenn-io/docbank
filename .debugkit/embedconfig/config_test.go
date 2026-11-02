package embedconfig_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedconfig"
)

func TestPreparedFillsOnlyOperationalDefaults(t *testing.T) {
	batch, err := embedconfig.Batch{}.Prepared()
	require.NoError(t, err)
	assert.Equal(t, embedconfig.DefaultBatchItems, batch.Items)
	transport, err := embedconfig.Transport{}.Prepared()
	require.NoError(t, err)
	assert.Equal(t, embedconfig.DefaultTimeout, transport.Timeout)
	assert.Equal(t, embedconfig.DefaultMaxResponseBytes, transport.MaxResponseBytes)

	batch, err = embedconfig.Batch{Items: 32, MaxTokens: 8192, InputTokenUpperBound: 512}.Prepared()
	require.NoError(t, err)
	assert.Equal(t, 32, batch.Items)
	transport, err = embedconfig.Transport{Timeout: 45 * time.Second}.Prepared()
	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, transport.Timeout)

	model, err := embedconfig.Model{Name: " bge-m3 ", Dimensions: 1024, Metric: " cosine ", Normalization: embedconfig.NormalizationL2}.Prepared()
	require.NoError(t, err)
	assert.Equal(t, "bge-m3", model.Name)
	assert.Equal(t, embedconfig.MetricCosine, model.Metric)
	roles, err := embedconfig.Roles{}.Prepared()
	require.NoError(t, err)
	assert.Equal(t, embedconfig.InputTypeNone, roles.InputType)
}

func TestPreparedRejectsIncompleteSettings(t *testing.T) {
	model := embedconfig.Model{Name: "m", Dimensions: 3, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2}
	tests := []struct {
		name    string
		prepare func() error
	}{
		{name: "missing dimensions", prepare: func() error {
			m := model
			m.Dimensions = 0
			_, err := m.Prepared()
			return err
		}},
		{name: "missing metric", prepare: func() error {
			m := model
			m.Metric = ""
			_, err := m.Prepared()
			return err
		}},
		{name: "negative batch", prepare: func() error {
			_, err := embedconfig.Batch{Items: -1}.Prepared()
			return err
		}},
		{name: "token bound exceeds cap", prepare: func() error {
			_, err := embedconfig.Batch{MaxTokens: 10, InputTokenUpperBound: 11}.Prepared()
			return err
		}},
		{name: "negative timeout", prepare: func() error {
			_, err := embedconfig.Transport{Timeout: -time.Second}.Prepared()
			return err
		}},
		{name: "token window without truncation", prepare: func() error {
			_, err := embedconfig.InputLimits{MaxTokens: 128, Tokenizer: "piece"}.Prepared()
			return err
		}},
		{name: "plaintext public endpoint", prepare: func() error {
			_, err := embedconfig.Deployment{BaseURL: "http://example.test/v1"}.Prepared()
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, test.prepare())
		})
	}
}

func TestModelRejectsMetricsTheVectorPipelineCannotStore(t *testing.T) {
	for _, metric := range []embedconfig.Metric{embedconfig.MetricDotProduct, embedconfig.MetricL2} {
		model := cosineModel()
		model.Metric = metric
		err := model.Validate()
		require.Error(t, err)
		require.ErrorContains(t, err, "not storable")
	}
	require.NoError(t, cosineModel().Validate())
}

func TestMaxSpansZeroIsUnlimited(t *testing.T) {
	window := embedconfig.InputLimits{
		Recipe:     "v2",
		Tokenizer:  "bge-m3",
		ContentID:  "body",
		MaxTokens:  512,
		Truncation: embedconfig.TruncationReject,
		MaxSpans:   0,
	}
	require.NoError(t, window.Validate(), "MaxSpans 0 is valid and means no span cap")
}

func TestCanonicalEndpoint(t *testing.T) {
	got, err := embedconfig.CanonicalEndpoint("HTTPS://Example.TEST:443/v1/", false)
	require.NoError(t, err)
	assert.Equal(t, "https://example.test/v1", got)

	_, err = embedconfig.CanonicalEndpoint("https://user:secret@example.test/v1", false)
	require.Error(t, err)
	_, err = embedconfig.CanonicalEndpoint("https://example.test/v1?q=1", false)
	require.Error(t, err)

	_, err = embedconfig.CanonicalEndpoint("http://10.1.2.3:8080/v1", false)
	require.Error(t, err)
	trusted, err := embedconfig.CanonicalEndpoint("http://10.1.2.3:8080/v1", true)
	require.NoError(t, err)
	assert.Equal(t, "http://10.1.2.3:8080/v1", trusted)

	cgnat, err := embedconfig.CanonicalEndpoint("http://100.64.0.8/v1", true)
	require.NoError(t, err)
	assert.Equal(t, "http://100.64.0.8/v1", cgnat)

	loopback, err := embedconfig.CanonicalEndpoint("http://127.0.0.1:8080/v1", false)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:8080/v1", loopback)

	_, err = embedconfig.CanonicalEndpoint("http://gpu-box.local/v1", false)
	require.Error(t, err)
	named, err := embedconfig.CanonicalEndpoint("http://gpu-box.local:11434/v1", true)
	require.NoError(t, err)
	assert.Equal(t, "http://gpu-box.local:11434/v1", named)

	_, err = embedconfig.CanonicalEndpoint("https://example.test/v1/../admin", false)
	require.Error(t, err)
	_, err = embedconfig.CanonicalEndpoint("https://example.test/v1/%2e%2e/admin", false)
	require.Error(t, err)
	dotted, err := embedconfig.CanonicalEndpoint("https://example.test/v1..2/models/", false)
	require.NoError(t, err)
	assert.Equal(t, "https://example.test/v1..2/models", dotted)
}

func TestCanonicalEndpointHostAndIPv6Zone(t *testing.T) {
	_, err := embedconfig.CanonicalEndpoint("https://:443/v1", false)
	require.Error(t, err)
	portOnly, err := url.Parse("https://:443/v1")
	require.NoError(t, err)
	_, err = embedconfig.Origin(portOnly)
	require.Error(t, err)

	plain, err := embedconfig.CanonicalEndpoint("https://[::1]:443/v1/", false)
	require.NoError(t, err)
	assert.Equal(t, "https://[::1]/v1", plain)

	const want = "https://[fe80::1%25Eth0]/v1"
	got, err := embedconfig.CanonicalEndpoint("https://[fe80::1%Eth0]/v1", false)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	encoded, err := embedconfig.CanonicalEndpoint("https://[FE80::1%25Eth0]/v1/", false)
	require.NoError(t, err)
	assert.Equal(t, want, encoded)
	again, err := embedconfig.CanonicalEndpoint(got, false)
	require.NoError(t, err)
	assert.Equal(t, got, again)
	assert.Contains(t, again, "Eth0")

	parsed, err := url.Parse(got)
	require.NoError(t, err)
	origin, err := embedconfig.Origin(parsed)
	require.NoError(t, err)
	assert.Equal(t, "https://[fe80::1%25Eth0]", origin)

	_, err = embedconfig.CanonicalEndpoint("http://[fe80::1%eth0]/v1", false)
	require.Error(t, err)
	trusted, err := embedconfig.CanonicalEndpoint("http://[fe80::1%Eth0]:8080/v1/", true)
	require.NoError(t, err)
	assert.Equal(t, "http://[fe80::1%25Eth0]:8080/v1", trusted)
	trustedAgain, err := embedconfig.CanonicalEndpoint(trusted, true)
	require.NoError(t, err)
	assert.Equal(t, trusted, trustedAgain)
}

func cosineModel() embedconfig.Model {
	return embedconfig.Model{
		Name:          "text-embedding-3-small",
		Dimensions:    1536,
		Metric:        embedconfig.MetricCosine,
		Normalization: embedconfig.NormalizationL2,
	}
}
