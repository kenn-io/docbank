package geminiembed

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"

	"go.kenn.io/docbank/document"
)

const (
	maximumInlineBytes    = int64(100 << 20)
	maximumInlinePDFBytes = int64(50 << 20)
	maximumVideoFrames    = int64(10_000)
)

type verifiedFile struct {
	data     []byte
	metadata document.AuthorizedUploadMetadata
	facts    document.VerifiedUploadFacts
}

type enrolledUpload struct {
	source     document.AuthorizedUpload
	metadata   document.AuthorizedUploadMetadata
	proof      document.VerifiedUploadProof
	facts      document.VerifiedUploadFacts
	proofFound bool

	closeOnce sync.Once
	closeErr  error
}

func (upload *enrolledUpload) Close() error {
	upload.closeOnce.Do(func() { upload.closeErr = upload.source.Close() })
	return upload.closeErr
}

func (upload *enrolledUpload) liveProof() (document.VerifiedUploadProof, bool) {
	carrier, ok := upload.source.(document.VerifiedUploadProofCarrier)
	if !ok {
		return document.VerifiedUploadProof{}, false
	}
	return carrier.VerifiedUploadProof()
}

type frozenUpload struct{ enrolled *enrolledUpload }

func (upload *frozenUpload) Read(buffer []byte) (int, error) {
	return upload.enrolled.source.Read(buffer)
}
func (upload *frozenUpload) Close() error { return upload.enrolled.Close() }
func (upload *frozenUpload) Metadata() document.AuthorizedUploadMetadata {
	return upload.enrolled.metadata
}
func (upload *frozenUpload) VerifiedUploadProof() (document.VerifiedUploadProof, bool) {
	if !upload.enrolled.proofFound {
		return document.VerifiedUploadProof{}, false
	}
	return upload.enrolled.proof, true
}

func enrollOriginalUploads(inputs []document.EmbeddingInput) ([]document.EmbeddingInput, []*enrolledUpload, error) {
	frozen := slices.Clone(inputs)
	enrolled := make([]*enrolledUpload, 0, len(inputs))
	owners := make([]*enrolledUpload, len(inputs))
	seen := make(map[document.AuthorizedUpload]*enrolledUpload, len(inputs))
	var enrollmentErr error
	for index := range frozen {
		if len(frozen[index].HeadingPath) > maximumHeadingDepth || len(frozen[index].SourceSpans) > maximumSourceSpanCount {
			frozen[index].HeadingPath = nil
			frozen[index].SourceSpans = nil
			if enrollmentErr == nil {
				enrollmentErr = errors.New("gemini embed: embedding auxiliary bounds are invalid")
			}
		} else {
			frozen[index].HeadingPath = slices.Clone(frozen[index].HeadingPath)
			frozen[index].SourceSpans = slices.Clone(frozen[index].SourceSpans)
		}
		source := frozen[index].Source
		if source == nil || nilInterface(source) {
			if frozen[index].Kind == document.EmbeddingInputOriginalFile && enrollmentErr == nil {
				enrollmentErr = errors.New("gemini embed: original upload source is nil")
			}
			continue
		}
		identity := reflect.ValueOf(source)
		if !identity.Comparable() || identity.Kind() == reflect.Pointer && identity.Type().Elem().Size() == 0 {
			upload := &enrolledUpload{source: source}
			enrolled = append(enrolled, upload)
			owners[index] = upload
			if enrollmentErr == nil {
				enrollmentErr = errors.New("gemini embed: original upload identity is not safely comparable")
			}
			continue
		}
		if upload, duplicate := seen[source]; duplicate {
			owners[index] = upload
			if enrollmentErr == nil {
				enrollmentErr = errors.New("gemini embed: original upload source is repeated")
			}
			continue
		}
		upload := &enrolledUpload{source: source}
		enrolled = append(enrolled, upload)
		owners[index] = upload
		seen[source] = upload
		if (frozen[index].Role != document.EmbeddingRoleDocument ||
			frozen[index].Kind != document.EmbeddingInputOriginalFile) && enrollmentErr == nil {
			enrollmentErr = errors.New("gemini embed: upload source is attached to an unsupported input role or kind")
		}
	}
	if enrollmentErr != nil {
		return frozen, enrolled, enrollmentErr
	}
	for _, upload := range enrolled {
		upload.metadata = upload.source.Metadata()
		upload.proof, upload.proofFound = upload.liveProof()
		if upload.proofFound {
			upload.facts = upload.proof.Snapshot()
		}
	}
	for index, upload := range owners {
		if upload != nil {
			frozen[index].Source = &frozenUpload{enrolled: upload}
		}
	}
	return frozen, enrolled, nil
}

func closeEnrolledUploads(uploads []*enrolledUpload) error {
	var result error
	for _, upload := range uploads {
		result = errors.Join(result, upload.Close())
	}
	return result
}

type activeSourceGate struct {
	mu          sync.Mutex
	canceled    bool
	nextToken   uint64
	activeToken uint64
	active      *enrolledUpload
}

func newActiveSourceGate() *activeSourceGate { return new(activeSourceGate) }

func (gate *activeSourceGate) Begin(source *enrolledUpload) (uint64, bool) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.canceled {
		return 0, false
	}
	gate.nextToken++
	gate.activeToken = gate.nextToken
	gate.active = source
	return gate.activeToken, true
}

func (gate *activeSourceGate) End(token uint64) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.activeToken != token {
		return
	}
	gate.active = nil
	gate.activeToken = 0
}

func (gate *activeSourceGate) Cancel() {
	gate.mu.Lock()
	if gate.canceled {
		gate.mu.Unlock()
		return
	}
	gate.canceled = true
	active := gate.active
	gate.active = nil
	gate.activeToken = 0
	gate.mu.Unlock()
	if active != nil {
		_ = document.InterruptAuthorizedUpload(active.source)
	}
}

func (client *Client) validateDirectCapability(upload *enrolledUpload, authorization document.EmbeddingAuthorization) error {
	if !upload.proofFound {
		return errors.New("gemini embed: original upload lacks verified proof")
	}
	if !upload.proof.Valid() {
		return errors.New("gemini embed: original upload proof is invalid")
	}
	facts := upload.facts
	metadata := upload.metadata
	if facts.SourceBytes != metadata.ByteLength || facts.SourceSHA256 != metadata.SHA256 ||
		facts.CapabilityRecordChecksum != metadata.CapabilityRecordChecksum ||
		facts.MediaFamily != metadata.MediaFamily || facts.MediaType != metadata.MediaType ||
		facts.InputKind != string(metadata.InputKind) || metadata.InputKind != document.RenditionInputOriginalFile ||
		facts.DescriptorFingerprint != client.descriptor.Fingerprint ||
		facts.ProfileFingerprint != client.profile.CapabilityProfileFingerprint ||
		facts.DisclosureFingerprint != client.profile.DisclosureFingerprint ||
		authorization.DiscloseFilename != (metadata.Filename != "") {
		return errors.New("gemini embed: original upload proof does not match metadata, profile, or disclosure")
	}
	if facts.SourceBytes > authorization.MaxInputBytes || facts.SourceBytes > client.profile.MaxInputBytes ||
		facts.MaxSourceBytes > client.profile.MaxInputBytes {
		return errors.New("gemini embed: direct-file source exceeds byte capacity")
	}
	if !geminiCapabilitySupported(facts) {
		return errors.New("gemini embed: direct-file capability is unsupported or over limit")
	}
	return nil
}

func geminiCapabilitySupported(facts document.VerifiedUploadFacts) bool {
	switch facts.MediaFamily {
	case "image":
		return (facts.MediaType == "image/png" || facts.MediaType == "image/jpeg") &&
			facts.Pixels > 0 && facts.Frames > 0 && facts.MaxPixels > 0 && facts.MaxFrames > 0
	case "audio":
		return (facts.MediaType == "audio/wav" || facts.MediaType == "audio/mpeg") &&
			facts.DurationMS > 0 && facts.DurationMS <= 180_000 &&
			facts.MaxDurationMS > 0 && facts.MaxDurationMS <= 180_000
	case "video":
		return (facts.MediaType == "video/mp4" || facts.MediaType == "video/quicktime") &&
			facts.DurationMS > 0 && facts.DurationMS <= 120_000 && facts.Frames > 0 &&
			facts.Frames <= maximumVideoFrames && facts.Pixels > 0 &&
			facts.MaxDurationMS > 0 && facts.MaxDurationMS <= 120_000 &&
			facts.MaxFrames > 0 && facts.MaxFrames <= maximumVideoFrames && facts.MaxPixels > 0
	case "pdf":
		return facts.MediaType == "application/pdf" && facts.Pages > 0 && facts.Pages <= 6 &&
			facts.MaxPages > 0 && facts.MaxPages <= 6
	default:
		return false
	}
}

func (client *Client) preflightInlineRequest(mediaType string, sourceBytes int64) error {
	empty, err := json.Marshal(wireRequest{Model: "models/" + Model,
		Content:              wireContent{Parts: []wirePart{{InlineData: &wireInlineData{MIMEType: mediaType}}}},
		OutputDimensionality: client.descriptor.Dimension})
	if err != nil {
		return errors.New("gemini embed: request encoding failed")
	}
	defer clear(empty)
	encodedBytes := int64(base64.StdEncoding.EncodedLen(int(sourceBytes)))
	requestBytes := int64(len(empty)) + encodedBytes
	if requestBytes > client.profile.MaxRequestBytes {
		return errors.New("gemini embed: embedding request exceeds profile byte capacity")
	}
	providerLimit := maximumInlineBytes
	if mediaType == "application/pdf" {
		providerLimit = maximumInlinePDFBytes
	}
	if requestBytes > providerLimit {
		return errors.New("gemini embed: inline request exceeds provider byte capacity")
	}
	return nil
}

func (client *Client) readDirectFile(ctx context.Context, upload *enrolledUpload, gate *activeSourceGate) (verifiedFile, error) {
	if live := upload.source.Metadata(); live != upload.metadata {
		return verifiedFile{}, errors.New("gemini embed: direct-file upload metadata changed")
	}
	if live, present := upload.liveProof(); !present || live != upload.proof || live.Snapshot() != upload.facts {
		return verifiedFile{}, errors.New("gemini embed: direct-file proof authority changed")
	}
	token, ok := gate.Begin(upload)
	if !ok {
		if contextErr := ctx.Err(); contextErr != nil {
			return verifiedFile{}, contextErr
		}
		return verifiedFile{}, errors.New("gemini embed: direct-file source transfer stopped")
	}
	data, readErr := io.ReadAll(io.LimitReader(upload.source, upload.metadata.ByteLength+1))
	gate.End(token)
	metadataChanged := upload.source.Metadata() != upload.metadata
	proof, proofPresent := upload.liveProof()
	proofChanged := !proofPresent || proof != upload.proof || proof.Snapshot() != upload.facts
	closeErr := upload.Close()
	if contextErr := ctx.Err(); contextErr != nil {
		clear(data)
		return verifiedFile{}, contextErr
	}
	if readErr != nil {
		clear(data)
		return verifiedFile{}, fmt.Errorf("gemini embed: direct-file source read failed: %w", readErr)
	}
	if closeErr != nil {
		clear(data)
		return verifiedFile{}, fmt.Errorf("gemini embed: close direct-file source: %w", closeErr)
	}
	if metadataChanged || proofChanged || int64(len(data)) != upload.metadata.ByteLength {
		clear(data)
		return verifiedFile{}, errors.New("gemini embed: direct-file source changed or could not be read exactly")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != upload.metadata.SHA256 {
		clear(data)
		return verifiedFile{}, errors.New("gemini embed: direct-file source checksum changed")
	}
	return verifiedFile{data: data, metadata: upload.metadata, facts: upload.facts}, nil
}
