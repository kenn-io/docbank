package media

import (
	"errors"
	"fmt"
	"io"
)

// DefaultMaxPixels is the default per-axis and total pixel ceiling.
const DefaultMaxPixels = int64(16_000_000)

// Policy bounds which detected media is eligible for further processing.
// Zero MaxBytes and MaxPixels use the package defaults; zero MaxDurationMS
// applies no duration cap.
type Policy struct {
	// MaxBytes is the largest eligible input; it cannot exceed MaxBytes.
	MaxBytes int64 `json:"max_bytes"`
	// MaxPixels bounds width, height, and their product.
	MaxPixels int64 `json:"max_pixels"`
	// MaxDurationMS bounds video duration; zero applies no cap. Under a cap,
	// video whose duration cannot be measured is refused as too long.
	MaxDurationMS int64 `json:"max_duration_ms,omitempty"`
	// AllowStill admits single-frame images.
	AllowStill bool `json:"allow_still"`
	// AllowAnimated admits multi-frame images.
	AllowAnimated bool `json:"allow_animated"`
	// AllowVideo admits video.
	AllowVideo bool `json:"allow_video"`
}

// DefaultPolicy admits still images and video up to MaxBytes and
// DefaultMaxPixels, and excludes animated images.
func DefaultPolicy() Policy {
	return Policy{MaxBytes: MaxBytes, MaxPixels: DefaultMaxPixels, AllowStill: true, AllowVideo: true}
}

// Normalized returns the policy with defaults applied.
func (p Policy) Normalized() Policy {
	if p.MaxBytes <= 0 {
		p.MaxBytes = MaxBytes
	}
	if p.MaxPixels <= 0 {
		p.MaxPixels = DefaultMaxPixels
	}
	if p.MaxDurationMS < 0 {
		p.MaxDurationMS = 0
	}
	return p
}

// Validate reports whether the policy is usable.
func (p Policy) Validate() error {
	p = p.Normalized()
	if p.MaxBytes > MaxBytes {
		return fmt.Errorf("media: policy max bytes %d exceeds %d", p.MaxBytes, MaxBytes)
	}
	if !p.AllowStill && !p.AllowAnimated && !p.AllowVideo {
		return errors.New("media: policy admits no media kind")
	}
	return nil
}

// Reason is a stable, machine-readable eligibility outcome.
type Reason string

// Eligibility outcomes.
const (
	ReasonEligible           Reason = "eligible"
	ReasonUnsupportedMedia   Reason = "unsupported_media_type"
	ReasonMalformedMedia     Reason = "malformed_media"
	ReasonTooLarge           Reason = "too_large"
	ReasonTooManyPixels      Reason = "too_many_pixels"
	ReasonTooLong            Reason = "too_long"
	ReasonStillNotAllowed    Reason = "still_not_allowed"
	ReasonAnimatedNotAllowed Reason = "animated_not_allowed"
	ReasonVideoNotAllowed    Reason = "video_not_allowed"
)

// Evaluate applies policy to detected metadata.
func Evaluate(metadata Metadata, policy Policy) Reason {
	policy = policy.Normalized()
	if metadata.Size > policy.MaxBytes {
		return ReasonTooLarge
	}
	if metadata.Width <= 0 || metadata.Height <= 0 {
		return ReasonMalformedMedia
	}
	switch metadata.Kind {
	case KindImage:
		if metadata.Animated && !policy.AllowAnimated {
			return ReasonAnimatedNotAllowed
		}
		if !metadata.Animated && !policy.AllowStill {
			return ReasonStillNotAllowed
		}
	case KindVideo:
		if !policy.AllowVideo {
			return ReasonVideoNotAllowed
		}
		if policy.MaxDurationMS > 0 && (!metadata.DurationKnown || metadata.DurationMS > policy.MaxDurationMS) {
			return ReasonTooLong
		}
	default:
		return ReasonUnsupportedMedia
	}
	if metadata.Width > policy.MaxPixels || metadata.Height > policy.MaxPixels ||
		metadata.Width*metadata.Height > policy.MaxPixels {
		return ReasonTooManyPixels
	}
	return ReasonEligible
}

// Inspect detects and evaluates one input. Detection failures become the
// matching Reason rather than an error so callers can record stable outcomes;
// only read failures are returned as errors. Inputs longer than the policy
// limit are refused before they are read.
func Inspect(reader io.ReaderAt, size int64, declaredMediaType string, policy Policy) (Metadata, Reason, error) {
	policy = policy.Normalized()
	if size > policy.MaxBytes {
		return Metadata{Size: size, DeclaredMediaType: declaredMediaType}, ReasonTooLarge, nil
	}
	metadata, err := Detect(reader, size, declaredMediaType)
	if err != nil {
		reason, known := detectionReason(err)
		if !known {
			return Metadata{}, "", err
		}
		return Metadata{Size: size, DeclaredMediaType: declaredMediaType}, reason, nil
	}
	return metadata, Evaluate(metadata, policy), nil
}

// InspectBytes is Inspect for in-memory input.
func InspectBytes(data []byte, declaredMediaType string, policy Policy) (Metadata, Reason) {
	policy = policy.Normalized()
	if int64(len(data)) > policy.MaxBytes {
		return Metadata{Size: int64(len(data)), DeclaredMediaType: declaredMediaType}, ReasonTooLarge
	}
	metadata, err := DetectBytes(data, declaredMediaType)
	if err != nil {
		reason, known := detectionReason(err)
		if !known {
			reason = ReasonMalformedMedia
		}
		return Metadata{Size: int64(len(data)), DeclaredMediaType: declaredMediaType}, reason
	}
	return metadata, Evaluate(metadata, policy)
}

func detectionReason(err error) (Reason, bool) {
	switch {
	case errors.Is(err, ErrUnsupportedMedia):
		return ReasonUnsupportedMedia, true
	case errors.Is(err, ErrMalformedMedia):
		return ReasonMalformedMedia, true
	case errors.Is(err, ErrTooLarge):
		return ReasonTooLarge, true
	default:
		return "", false
	}
}
