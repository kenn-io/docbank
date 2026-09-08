package embeddingbridge_test

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

type capturedEmbeddingUpload struct {
	body             []byte
	manifest         requestManifest
	filePartFilename string
}

func TestClientFilenameDisclosure(t *testing.T) {
	for _, disclose := range []bool{false, true} {
		disclosureName := "withheld"
		if disclose {
			disclosureName = "disclosed"
		}
		for _, throughCore := range []bool{false, true} {
			executionName := "direct"
			if throughCore {
				executionName = "through_core"
			}
			t.Run(disclosureName+"/"+executionName, func(t *testing.T) {
				requestValidator := compileContractSchema(t, "https://docbank.invalid/contracts/docbank-embedding/v1/request.schema.json", requestSchema)
				responseValidator := compileContractSchema(t, "https://docbank.invalid/contracts/docbank-embedding/v1/response.schema.json", responseSchema)
				var requestContract struct {
					Canonicalization canonicalizationContract `json:"x-docbank-canonicalization"`
				}
				require.NoError(t, stdjson.Unmarshal(requestSchema, &requestContract))

				captured := make(chan capturedEmbeddingUpload, 1)
				serverErrors := make(chan error, 1)
				fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						serverErrors <- err
						http.Error(writer, "synthetic capture failure", http.StatusInternalServerError)
						return
					}
					observation, err := inspectCapturedEmbeddingUpload(request, body)
					if err != nil {
						serverErrors <- err
						http.Error(writer, "synthetic capture failure", http.StatusInternalServerError)
						return
					}
					captured <- observation
					request.Body = io.NopCloser(bytes.NewReader(body))
					if err := serveIndependentContractRequest(writer, request, requestValidator, responseValidator, requestContract.Canonicalization); err != nil {
						serverErrors <- err
						http.Error(writer, "synthetic contract failure", http.StatusInternalServerError)
					}
				}))

				source := []byte("synthetic disclosure source")
				metadata := uploadMetadata(source)
				metadata.Filename = "synthetic-source.bin"
				upload := newObservedUpload(source, metadata)
				authorization := fixture.authorization(1)
				authorization.DiscloseFilename = disclose
				inputs := []document.EmbeddingInput{{
					Key: "source", Role: document.EmbeddingRoleDocument,
					Kind: document.EmbeddingInputOriginalFile, Source: upload,
				}}
				var err error
				if throughCore {
					_, err = document.ExecuteEmbedding(t.Context(), fixture.client, inputs, authorization)
				} else {
					_, err = fixture.client.Embed(t.Context(), inputs, authorization)
				}
				require.NoError(t, err)
				select {
				case serverErr := <-serverErrors:
					require.NoError(t, serverErr)
				default:
				}
				observation := <-captured
				wantFilename := ""
				if disclose {
					wantFilename = metadata.Filename
				}
				require.Len(t, observation.manifest.Inputs, 1)
				require.NotNil(t, observation.manifest.Inputs[0].Upload)
				assert.Equal(t, wantFilename, observation.manifest.Inputs[0].Upload.Filename)
				assert.Equal(t, wantFilename, observation.filePartFilename)
				assert.Equal(t, metadata.Filename, upload.Metadata().Filename)
				if !disclose {
					assert.NotContains(t, string(observation.body), metadata.Filename)
				}
				if throughCore {
					assert.Positive(t, upload.closes.Load(), "core execution owns and closes the source")
				} else {
					assert.Zero(t, upload.closes.Load(), "completed direct sources remain caller-owned")
				}
			})
		}
	}
}

func inspectCapturedEmbeddingUpload(request *http.Request, body []byte) (capturedEmbeddingUpload, error) {
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil {
		return capturedEmbeddingUpload{}, fmt.Errorf("parse captured request media type: %w", err)
	}
	if mediaType != "multipart/form-data" || parameters["boundary"] == "" {
		return capturedEmbeddingUpload{}, errors.New("captured request is not multipart/form-data")
	}
	reader := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	observation := capturedEmbeddingUpload{body: bytes.Clone(body)}
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			return observation, nil
		}
		if nextErr != nil {
			return capturedEmbeddingUpload{}, fmt.Errorf("read captured multipart part: %w", nextErr)
		}
		switch part.FormName() {
		case "manifest":
			if err := stdjson.NewDecoder(part).Decode(&observation.manifest); err != nil {
				return capturedEmbeddingUpload{}, err
			}
		case "file":
			observation.filePartFilename = part.FileName()
			if _, err := io.Copy(io.Discard, part); err != nil {
				return capturedEmbeddingUpload{}, err
			}
		}
	}
}
