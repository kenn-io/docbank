package embedconfig_test

import (
	"fmt"

	"github.com/BurntSushi/toml"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

// ExampleEmbedder_textRetrieval configures the text path for a caller-managed
// OpenAI-compatible deployment. The native width and retrieval recipe come
// from https://ai.google.dev/gemma/docs/embeddinggemma/model_card_2.
// This example constructs a client without making an inference request;
// transport tests use synthetic responses. Actual serving and prompt behavior
// remain untested here.
//
// The operator must bind the example alias to the chosen checkpoint, tokenizer,
// mean pooling including prompts, and bfloat16 or float32 activations. Neither
// the alias nor fingerprint_salt verifies server weights. The server must
// enforce the 8192-token limit including affixes and must not add prompts again.
// model_context_tokens is only a conservative batching bound, not token
// admission control.
//
// For a reduced width, set dims to the returned width and request_dimensions
// to true only if the endpoint supports that request and performs truncation
// with L2 renormalization. Kit validates width and normalizes accepted vectors;
// it never slices them. EmbedTexts applies affixes to raw text; EncodeFunc
// sends already formatted text unchanged.
func ExampleEmbedder_textRetrieval() {
	var config embedconfig.Embedder
	_, err := toml.Decode(`
base_url = "http://127.0.0.1:8080/v1"
model = "embeddinggemma-2-text-r914f7f8"
fingerprint_salt = "google/embeddinggemma-2@914f7f89142e33e77833254d9c9b90c3cef7303b"
dims = 768
document_prefix = "title: none | text: "
query_prefix = "task: search result | query: "
request_dimensions = false
`, &config)
	if err != nil {
		panic(err)
	}
	parts, err := config.Parts()
	if err != nil {
		panic(err)
	}
	client, err := embedclient.New(embedclient.Options{
		Model: parts.Model, Roles: parts.Roles, Deployment: parts.Deployment,
		Batch: parts.Batch, Transport: parts.Transport,
	})
	if err != nil {
		panic(err)
	}
	for _, role := range []embedconfig.Role{embedconfig.RoleDocument, embedconfig.RoleQuery} {
		text, err := embedmodel.Format(role, "hello", parts.Roles)
		if err != nil {
			panic(err)
		}
		fmt.Println(text)
	}
	fmt.Println(parts.Model.Dimensions, parts.Model.RequestDimensions, client != nil)
	// Output:
	// title: none | text: hello
	// task: search result | query: hello
	// 768 false true
}
