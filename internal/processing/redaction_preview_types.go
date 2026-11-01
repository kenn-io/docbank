package processing

import (
	"errors"
	"time"
)

const MaxRedactionPreviewPNGBytes int64 = 128 << 20
const redactionPreviewLifetime = 30 * time.Minute

var (
	ErrRedactionPreviewBusy        = errors.New("a temporary redaction preview is already open; discard it or wait for expiry")
	ErrRedactionPreviewUnavailable = errors.New("redaction preview expired or is unavailable; create a new preview")
)

type RedactionPreviewPage struct {
	Number      int                 `json:"number"`
	FrameSHA256 string              `json:"frame_sha256"`
	Width       int64               `json:"width"`
	Height      int64               `json:"height"`
	PixelWidth  int                 `json:"pixel_width"`
	PixelHeight int                 `json:"pixel_height"`
	PNG         RedactionFileDigest `json:"png"`
	TextStart   int                 `json:"text_start"`
	TextEnd     int                 `json:"text_end"`
}

type RedactionPreview struct {
	Contract  string                 `json:"contract"`
	ID        string                 `json:"id"`
	ExpiresAt time.Time              `json:"expires_at"`
	Archive   RedactionArchive       `json:"archive"`
	Pages     []RedactionPreviewPage `json:"pages"`
}
