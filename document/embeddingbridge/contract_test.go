package embeddingbridge_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	stdjson "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/embeddingbridge"
	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var openAPIContract []byte

//go:embed embedding-request-v1.schema.json
var requestSchema []byte

//go:embed embedding-response-v1.schema.json
var responseSchema []byte

func TestNormativeEmbeddingContractIsFixedStrictAndVersioned(t *testing.T) {
	var openAPI map[string]any
	require.NoError(t, yaml.Unmarshal(openAPIContract, &openAPI))
	assert.Equal(t, "3.1.0", openAPI["openapi"])
	paths, ok := openAPI["paths"].(map[string]any)
	require.True(t, ok)
	assert.Len(t, paths, 1)
	assert.Contains(t, paths, "/docbank-embedding/v1/embeddings")
	for _, forbidden := range []string{"{{", "result_url", "callback", "status_url", "cancel_url"} {
		assert.NotContains(t, string(openAPIContract), forbidden)
	}
	for name, schema := range map[string][]byte{"request": requestSchema, "response": responseSchema} {
		t.Run(name, func(t *testing.T) {
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(schema, &decoded))
			assert.Equal(t, "https://json-schema.org/draft/2020-12/schema", decoded["$schema"])
			assert.Equal(t, false, decoded["additionalProperties"])
			assert.True(t, bytes.Contains(schema, []byte(`"docbank-embedding/v1"`)))
		})
	}
	assert.Equal(t, "docbank-embedding/v1", embeddingbridge.ContractVersion)
	for _, field := range []string{`"heading_path"`, `"source_spans"`, `"unit_index"`, `"char_start"`, `"char_end"`} {
		assert.Contains(t, string(requestSchema), field)
	}
	var requestContract struct {
		Canonicalization canonicalizationContract `json:"x-docbank-canonicalization"`
	}
	require.NoError(t, json.Unmarshal(requestSchema, &requestContract))
	assert.Equal(t, "docbank-json-v1", requestContract.Canonicalization.Name)
	assert.Equal(t, "UTF-8 without a byte-order mark", requestContract.Canonicalization.CharacterEncoding)
	assert.NotEmpty(t, requestContract.Canonicalization.Whitespace)
	assert.NotEmpty(t, requestContract.Canonicalization.Arrays)
	assert.NotEmpty(t, requestContract.Canonicalization.Integers)
	assert.NotEmpty(t, requestContract.Canonicalization.Strings)
	assert.NotEmpty(t, requestContract.Canonicalization.OptionalMembers)
	assert.NotEmpty(t, requestContract.Canonicalization.Checksum)
	assert.Equal(t, []string{
		"contract_version", "descriptor_fingerprint", "policy_fingerprint", "authorization", "inputs", "request_checksum",
	}, requestContract.Canonicalization.ObjectMemberOrder["$"])
	require.NotEmpty(t, requestContract.Canonicalization.ChecksumVectors)
	for _, vector := range requestContract.Canonicalization.ChecksumVectors {
		_, wire, identity, err := canonicalizeIndependentManifest([]byte(vector.CanonicalManifest), requestContract.Canonicalization)
		require.NoError(t, err)
		assert.Equal(t, vector.CanonicalManifest, string(wire))
		assert.Equal(t, vector.CanonicalManifest, string(identity))
		digest := sha256.Sum256([]byte(vector.CanonicalManifest))
		assert.Equal(t, vector.SHA256, hex.EncodeToString(digest[:]))
		assert.NotContains(t, vector.CanonicalManifest, `"request_checksum"`)
	}
}

type canonicalizationContract struct {
	Name              string              `json:"name"`
	CharacterEncoding string              `json:"character_encoding"`
	Whitespace        string              `json:"whitespace"`
	ObjectMemberOrder map[string][]string `json:"object_member_order"`
	Arrays            string              `json:"arrays"`
	Integers          string              `json:"integers"`
	Strings           string              `json:"strings"`
	OptionalMembers   string              `json:"optional_members"`
	Checksum          string              `json:"checksum"`
	ChecksumVectors   []struct {
		CanonicalManifest string `json:"canonical_manifest"`
		SHA256            string `json:"sha256"`
	} `json:"checksum_vectors"`
}

func TestIndependentSyntheticServerImplementsPublishedSchema(t *testing.T) {
	requestValidator := compileContractSchema(t, "https://docbank.invalid/contracts/docbank-embedding/v1/request.schema.json", requestSchema)
	responseValidator := compileContractSchema(t, "https://docbank.invalid/contracts/docbank-embedding/v1/response.schema.json", responseSchema)
	var requestContract struct {
		Canonicalization canonicalizationContract `json:"x-docbank-canonicalization"`
	}
	require.NoError(t, json.Unmarshal(requestSchema, &requestContract))
	serverErrors := make(chan error, 1)
	fixture := newBridgeFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := serveIndependentContractRequest(writer, request, requestValidator, responseValidator, requestContract.Canonicalization); err != nil {
			select {
			case serverErrors <- err:
			default:
			}
			http.Error(writer, "synthetic contract failure", http.StatusInternalServerError)
		}
	}))
	source := []byte("synthetic independent file")
	sourceDigest := sha256.Sum256(source)
	upload := &contractUpload{
		reader: bytes.NewReader(source),
		metadata: document.AuthorizedUploadMetadata{
			Filename: "contract.bin", MediaFamily: "binary", MediaType: "application/octet-stream",
			ByteLength: int64(len(source)), SHA256: hex.EncodeToString(sourceDigest[:]),
			CapabilityRecordChecksum: strings.Repeat("a", 64), ProviderMetadataChecksum: strings.Repeat("b", 64),
			InputKind: document.RenditionInputOriginalFile,
		},
	}
	result, err := fixture.client.Embed(context.Background(), []document.EmbeddingInput{
		{
			Key: "first", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk,
			Text: "synthetic contract fixture", HeadingPath: []string{"Synthetic"},
			SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 9}},
		},
		{Key: "second", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: upload},
	}, fixture.authorization(2))
	select {
	case serverErr := <-serverErrors:
		require.NoError(t, serverErr)
	default:
	}
	require.NoError(t, err)
	assert.Equal(t, document.EmbeddingResult{Vectors: []document.EmbeddingVector{
		{Key: "first", Values: []float32{0.5, 0.25}},
		{Key: "second", Values: []float32{1.5, 1.25}},
	}}, result)
}

type contractUpload struct {
	reader   *bytes.Reader
	metadata document.AuthorizedUploadMetadata
}

func (upload *contractUpload) Read(value []byte) (int, error) {
	read, err := upload.reader.Read(value)
	if errors.Is(err, io.EOF) {
		return read, io.EOF
	}
	if err != nil {
		return read, fmt.Errorf("contract upload read: %w", err)
	}
	return read, nil
}

func (upload *contractUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

func (*contractUpload) Close() error { return nil }

type independentFilePart struct {
	filename  string
	mediaType string
	payload   []byte
}

func compileContractSchema(t *testing.T, location string, document []byte) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	require.NoError(t, compiler.AddResource(location, bytes.NewReader(document)))
	compiled, err := compiler.Compile(location)
	require.NoError(t, err)
	return compiled
}

func serveIndependentContractRequest(
	writer http.ResponseWriter,
	request *http.Request,
	requestValidator *jsonschema.Schema,
	responseValidator *jsonschema.Schema,
	canonicalization canonicalizationContract,
) error {
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || parameters["boundary"] == "" {
		return errors.New("independent server: multipart content type")
	}
	reader := multipart.NewReader(request.Body, parameters["boundary"])
	var manifestBytes []byte
	var files []independentFilePart
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("independent server: next part: %w", nextErr)
		}
		payload, readErr := io.ReadAll(part)
		if readErr != nil {
			return fmt.Errorf("independent server: read part: %w", readErr)
		}
		switch part.FormName() {
		case "manifest":
			if part.Header.Get("Content-Type") != "application/vnd.docbank.embedding-manifest+json;version=1" || manifestBytes != nil {
				return errors.New("independent server: manifest part contract")
			}
			manifestBytes = payload
		case "file":
			files = append(files, independentFilePart{filename: part.FileName(), mediaType: part.Header.Get("Content-Type"), payload: payload})
		default:
			return errors.New("independent server: unknown multipart part")
		}
	}
	if manifestBytes == nil {
		return errors.New("independent server: unexpected multipart shape")
	}
	var schemaValue any
	if err := stdjson.Unmarshal(manifestBytes, &schemaValue); err != nil {
		return fmt.Errorf("independent server: decode request schema value: %w", err)
	}
	if err := requestValidator.Validate(schemaValue); err != nil {
		return fmt.Errorf("independent server: request schema: %w", err)
	}
	manifest, wireCanonical, identityCanonical, err := canonicalizeIndependentManifest(manifestBytes, canonicalization)
	if err != nil {
		return err
	}
	if !bytes.Equal(manifestBytes, wireCanonical) {
		return errors.New("independent server: request is not canonical docbank-json-v1")
	}
	checksum, ok := manifest["request_checksum"].(string)
	if !ok {
		return errors.New("independent server: request checksum type")
	}
	digest := sha256.Sum256(identityCanonical)
	if checksum != hex.EncodeToString(digest[:]) || request.Header.Get("Idempotency-Key") != checksum || request.Header.Get("Docbank-Request-Checksum") != checksum {
		return errors.New("independent server: request checksum mismatch")
	}
	descriptorFingerprint, descriptorOK := manifest["descriptor_fingerprint"].(string)
	policyFingerprint, policyOK := manifest["policy_fingerprint"].(string)
	inputs, inputsOK := manifest["inputs"].([]any)
	if !descriptorOK || !policyOK || !inputsOK || len(inputs) == 0 {
		return errors.New("independent server: manifest identity")
	}
	keys := make([]string, len(inputs))
	usedFiles := make([]bool, len(files))
	for index, rawInput := range inputs {
		input, inputOK := rawInput.(map[string]any)
		key, keyOK := input["key"].(string)
		kind, kindOK := input["kind"].(string)
		if !inputOK || !keyOK || !kindOK {
			return errors.New("independent server: input identity")
		}
		keys[index] = key
		if kind != "original_file" {
			continue
		}
		fileIndex, indexErr := independentInteger(input["file_index"])
		upload, uploadOK := input["upload"].(map[string]any)
		if indexErr != nil || !uploadOK || fileIndex < 0 || fileIndex >= int64(len(files)) || usedFiles[fileIndex] {
			return errors.New("independent server: file index")
		}
		usedFiles[fileIndex] = true
		length, lengthErr := independentInteger(upload["byte_length"])
		checksum, checksumOK := upload["sha256"].(string)
		filename, filenameOK := upload["filename"].(string)
		fileMediaType, mediaTypeOK := upload["media_type"].(string)
		file := files[fileIndex]
		fileDigest := sha256.Sum256(file.payload)
		if lengthErr != nil || !checksumOK || !filenameOK || !mediaTypeOK || length != int64(len(file.payload)) ||
			checksum != hex.EncodeToString(fileDigest[:]) || filename != file.filename || fileMediaType != file.mediaType {
			return errors.New("independent server: file binding")
		}
	}
	for _, used := range usedFiles {
		if !used {
			return errors.New("independent server: unbound file")
		}
	}
	vectors := make([]any, len(keys))
	for index, key := range keys {
		vectors[index] = map[string]any{
			"key": key, "index": float64(index), "values": []any{float64(index) + 0.5, float64(index) + 0.25},
		}
	}
	response := map[string]any{
		"contract_version":       "docbank-embedding/v1",
		"descriptor_fingerprint": descriptorFingerprint,
		"policy_fingerprint":     policyFingerprint,
		"request_checksum":       checksum,
		"vectors":                vectors,
	}
	responseBytes, err := stdjson.Marshal(response)
	if err != nil {
		return fmt.Errorf("independent server: encode response: %w", err)
	}
	var responseWire any
	if err := stdjson.Unmarshal(responseBytes, &responseWire); err != nil {
		return fmt.Errorf("independent server: decode response wire: %w", err)
	}
	if err := responseValidator.Validate(responseWire); err != nil {
		return fmt.Errorf("independent server: response schema: %w", err)
	}
	writer.Header().Set("Content-Type", "application/vnd.docbank.embedding-result+json;version=1")
	if _, err := writer.Write(responseBytes); err != nil {
		return fmt.Errorf("independent server: write response: %w", err)
	}
	return nil
}

func canonicalizeIndependentManifest(payload []byte, contract canonicalizationContract) (map[string]any, []byte, []byte, error) {
	decoder := stdjson.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, nil, nil, fmt.Errorf("independent server: decode canonical manifest: %w", err)
	}
	if err := requireIndependentJSONEOF(decoder); err != nil {
		return nil, nil, nil, err
	}
	manifest, ok := decoded.(map[string]any)
	if !ok {
		return nil, nil, nil, errors.New("independent server: manifest root")
	}
	wire, err := appendIndependentCanonical(nil, manifest, "$", contract.ObjectMemberOrder)
	if err != nil {
		return nil, nil, nil, err
	}
	identityManifest := maps.Clone(manifest)
	delete(identityManifest, "request_checksum")
	identity, err := appendIndependentCanonical(nil, identityManifest, "$", contract.ObjectMemberOrder)
	if err != nil {
		return nil, nil, nil, err
	}
	return manifest, wire, identity, nil
}

func requireIndependentJSONEOF(decoder *stdjson.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("independent server: trailing manifest JSON")
	}
	return nil
}

func appendIndependentCanonical(destination []byte, value any, path string, orders map[string][]string) ([]byte, error) {
	switch value := value.(type) {
	case map[string]any:
		order, ok := orders[path]
		if !ok {
			return nil, fmt.Errorf("independent server: no member order for %s", path)
		}
		destination = append(destination, '{')
		members := 0
		for _, name := range order {
			member, exists := value[name]
			if !exists {
				continue
			}
			if members > 0 {
				destination = append(destination, ',')
			}
			var err error
			destination, err = jsontext.AppendQuote(destination, name)
			if err != nil {
				return nil, fmt.Errorf("independent server: quote member: %w", err)
			}
			destination = append(destination, ':')
			destination, err = appendIndependentCanonical(destination, member, path+"."+name, orders)
			if err != nil {
				return nil, err
			}
			members++
		}
		if members != len(value) {
			return nil, fmt.Errorf("independent server: unordered member at %s", path)
		}
		return append(destination, '}'), nil
	case []any:
		destination = append(destination, '[')
		for index, member := range value {
			if index > 0 {
				destination = append(destination, ',')
			}
			var err error
			destination, err = appendIndependentCanonical(destination, member, path+"[]", orders)
			if err != nil {
				return nil, err
			}
		}
		return append(destination, ']'), nil
	case string:
		encoded, err := jsontext.AppendQuote(destination, value)
		if err != nil {
			return nil, fmt.Errorf("independent server: quote value: %w", err)
		}
		return encoded, nil
	case stdjson.Number:
		if !isCanonicalUnsignedInteger(value.String()) {
			return nil, errors.New("independent server: non-canonical request integer")
		}
		return append(destination, value.String()...), nil
	default:
		return nil, fmt.Errorf("independent server: unsupported canonical value %T", value)
	}
}

func isCanonicalUnsignedInteger(value string) bool {
	if value == "0" {
		return true
	}
	if value == "" || value[0] < '1' || value[0] > '9' {
		return false
	}
	return strings.IndexFunc(value[1:], func(character rune) bool { return character < '0' || character > '9' }) < 0
}

func independentInteger(value any) (int64, error) {
	number, ok := value.(stdjson.Number)
	if !ok || !isCanonicalUnsignedInteger(number.String()) {
		return 0, errors.New("independent server: integer type")
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("independent server: parse integer: %w", err)
	}
	return parsed, nil
}
