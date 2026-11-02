// Package embedclient calls an OpenAI-compatible text embeddings endpoint.
//
// The caller owns the HTTP client when it supplies one. This package clones
// that client, pins requests to the configured origin, and can attach a bearer
// token. Failures are not retried unless the caller sets Options.Retry,
// which retries only failures that may succeed later. A non-2xx response, a
// short body, or a vector that fails validation fails the whole call.
// Response indexes are applied inside each request. A missing index on every item means the
// provider kept request order.
//
// Ollama on Apple Metal can answer HTTP 200 with a vector that is not a real
// number. Set OllamaMetalRecovery to keep the usable vectors and send only
// the bad inputs to Ollama's native embed route. That request unloads the
// current runner, retries it once, and then encodes those inputs once with
// the GPU off. Those recovery requests use a 30 minute timeout instead of
// the per-request timeout, because a CPU pass can take many minutes. The endpoint path must end in /v1. The switch is not part of the
// vector identity.
//
//	client, err := embedclient.New(embedclient.Options{
//	    Model: embedconfig.Model{
//	        Name: "nomic-embed-text", Dimensions: 768,
//	        Metric: embedconfig.MetricCosine,
//	        Normalization: embedconfig.NormalizationNone,
//	    },
//	    Deployment: embedconfig.Deployment{
//	        BaseURL: "http://127.0.0.1:11434/v1",
//	    },
//	    OllamaMetalRecovery: true,
//	})
//
// Leave the switch off for every other endpoint.
//
// New accepts only a cosine metric. A token budget lowers how many inputs
// share one request. Response embeddings are a JSON array of finite numbers
// or a base64 string of little-endian float32 values. The encoding format is
// empty, float, or base64.
//
// Failures are typed. APIError reports the status, a Reason derived from the
// status and the provider's error code or message, and whether a retry may
// help. The provider body itself is never kept. TransportError keeps the network cause behind a generic message.
// VectorError names the input whose vector failed validation, and matches
// ErrInvalidVector.
//
// Embed and EmbedTexts apply the role prefix and suffix to raw content.
// EncodeFunc sends its strings unchanged, so text from embedfit.Prepared is
// not wrapped a second time.
//
// Text is the only encoded form. Image and file content returns
// embedmodel.ErrUnsupportedContent so those callers can keep their own path.
// vector.EncodeFunc remains available through EncodeFunc.
package embedclient
