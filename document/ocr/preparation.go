package ocr

// PreparationError describes a local source or conversion failure without
// exposing source content, paths, or parser output. Use errors.As to find it
// inside a ProviderError or another wrapper. Its description is for display;
// use ProviderError.Kind and errors.Is for scheduling decisions.
type PreparationError struct {
	description string
	cause       error
}

// NewPreparationError pairs a trusted, fixed display description with a cause.
// description must not contain source content, paths, or another error's text.
// The cause remains available through Unwrap and may contain private data.
func NewPreparationError(description string, cause error) *PreparationError {
	return &PreparationError{description: description, cause: cause}
}

func (e *PreparationError) Error() string {
	if e == nil || e.description == "" {
		return "document preparation failed"
	}
	return e.description
}

func (e *PreparationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
