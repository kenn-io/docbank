package embedclient_test

import (
	"fmt"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

func ExampleNew_ollamaMetalRecovery() {
	client, err := embedclient.New(embedclient.Options{
		Model: embedconfig.Model{
			Name:          "nomic-embed-text",
			Dimensions:    768,
			Metric:        embedconfig.MetricCosine,
			Normalization: embedconfig.NormalizationNone,
		},
		Deployment: embedconfig.Deployment{
			BaseURL: "http://127.0.0.1:11434/v1",
		},
		OllamaMetalRecovery: true,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(client != nil)
	// Output:
	// true
}
