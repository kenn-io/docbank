package document

const (
	// VisualPreviewContractV1 identifies one canonical visual derivative of an
	// exact retained source version.
	VisualPreviewContractV1 = "visual-preview/v1"

	MaxVisualPreviewEdgePixels  = 32768
	MaxVisualPreviewFailureCode = 128
	MaxVisualPreviewFailureText = 4096
)

// VisualPreviewState describes the durable outcome of one deterministic
// preview recipe. Transient I/O and storage failures are not cataloged.
type VisualPreviewState string

const (
	VisualPreviewReady       VisualPreviewState = "ready"
	VisualPreviewUnsupported VisualPreviewState = "unsupported"
	VisualPreviewFailed      VisualPreviewState = "failed"
)

// VisualPreviewRecipeV1 is the complete identity of preview-producing
// behavior. ProcessorFingerprint covers decoder, frame-selection, scaling,
// color, and encoder implementations that can affect the exact output bytes.
type VisualPreviewRecipeV1 struct {
	ColorPolicy          string `json:"color_policy"`
	ContractVersion      string `json:"contract_version"`
	FramePolicy          string `json:"frame_policy"`
	MaxEdgePixels        int    `json:"max_edge_pixels"`
	OrientationPolicy    string `json:"orientation_policy"`
	OutputMediaType      string `json:"output_media_type"`
	ProcessorFingerprint string `json:"processor_fingerprint"`
}

// VisualPreviewOutputV1 identifies the exact retained preview bytes.
type VisualPreviewOutputV1 struct {
	BlobSHA256 string `json:"blob_sha256"`
	Height     int    `json:"height"`
	MediaType  string `json:"media_type"`
	Size       int64  `json:"size"`
	Width      int    `json:"width"`
}

// VisualPreviewFailureV1 is a deterministic terminal result. Detail is for
// operators; Code is the stable machine-readable classification.
type VisualPreviewFailureV1 struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// VisualPreviewV1 is the canonical result for one exact source and recipe.
// Exactly one of Output and Failure is populated according to State.
type VisualPreviewV1 struct {
	ContractVersion string                  `json:"contract_version"`
	Failure         *VisualPreviewFailureV1 `json:"failure,omitempty"`
	Output          *VisualPreviewOutputV1  `json:"output,omitempty"`
	Recipe          VisualPreviewRecipeV1   `json:"recipe"`
	SourceSHA256    string                  `json:"source_sha256"`
	State           VisualPreviewState      `json:"state"`
}
