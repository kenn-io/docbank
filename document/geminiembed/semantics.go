package geminiembed

// Semantics is the fixed provider-behavior snapshot disclosed by this adapter.
type Semantics struct {
	Contract              string `json:"contract"`
	PDFOCR                string `json:"pdf_ocr"`
	OverlongInput         string `json:"overlong_input"`
	VideoAudio            string `json:"video_audio"`
	MaxInputTokens        int64  `json:"max_input_tokens"`
	MaxPDFPages           int64  `json:"max_pdf_pages"`
	MaxImagesPerRequest   int64  `json:"max_images_per_request"`
	MaxAudioDurationMS    int64  `json:"max_audio_duration_ms"`
	MaxVideoDurationMS    int64  `json:"max_video_duration_ms"`
	MaxSampledVideoFrames int64  `json:"max_sampled_video_frames"`
}

func fixedSemantics() Semantics {
	return Semantics{
		Contract: "gemini-embedding-2/developer-api-semantics/v1", PDFOCR: "always_enabled",
		OverlongInput: "provider_may_truncate", VideoAudio: "ignored", MaxInputTokens: 8192,
		MaxPDFPages: 6, MaxImagesPerRequest: 6, MaxAudioDurationMS: 180000,
		MaxVideoDurationMS: 120000, MaxSampledVideoFrames: 32,
	}
}

// Semantics returns an immutable value snapshot of the fixed provider behavior.
func (client *Client) Semantics() Semantics {
	if client == nil {
		return Semantics{}
	}
	return fixedSemantics()
}
