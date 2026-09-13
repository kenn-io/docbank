package emailmime

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"golang.org/x/net/html/charset"
)

func (d *decoder) deriveBody(partPath, mediaType, charsetName, payloadName string) (refResult *document.EmailArtifactRefV1, stateResult document.EmailDisplayState, diagnosticsResult []document.EmailDiagnosticV1, resultErr error) {
	file, err := d.spool.openRegular(payloadName)
	if err != nil {
		return nil, document.EmailDisplayFailed, nil, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, &spoolIOError{err: closeErr})
		}
	}()
	var reader io.Reader
	var diagnostics []document.EmailDiagnosticV1
	validateUTF8 := false
	validateASCII := false
	charsetName = strings.TrimSpace(strings.ToLower(charsetName))
	switch charsetName {
	case "":
		diagnostics = append(diagnostics, d.diagnostic(document.EmailDiagnosticCharsetMissing, document.EmailOperationCharset, partPath, nil, "text body has no declared charset")...)
		reader = infrastructureMarkingReader{reader: file}
		validateUTF8 = true
	case "utf-8", "utf8":
		reader = infrastructureMarkingReader{reader: file}
		validateUTF8 = true
	case "us-ascii", "ascii":
		reader = infrastructureMarkingReader{reader: file}
		validateASCII = true
	default:
		reader, err = charset.NewReaderLabel(charsetName, infrastructureMarkingReader{reader: file})
		if err != nil {
			diagnostics = append(diagnostics, d.diagnostic(document.EmailDiagnosticCharsetUnsupported, document.EmailOperationCharset, partPath, nil, "declared charset is unsupported")...)
			return nil, document.EmailDisplayUnsupported, diagnostics, nil
		}
	}
	limit := d.limits.BodyUTF8Bytes
	limitCode := document.EmailDiagnosticBodyUTF8Limit
	if mediaType == "text/html" && d.limits.HTMLDisplayBytes < limit {
		limit = d.limits.HTMLDisplayBytes
		limitCode = document.EmailDiagnosticHTMLDisplayLimit
	}
	ref, err := d.storeStreamLimited(partPath, document.EmailArtifactBodyUTF8, reader, limit, &d.bodyUTF8Bytes, d.limits.AggregateBodyUTF8Bytes, limitCode, document.EmailDiagnosticBodyUTF8TotalLimit, document.EmailOperationCharset)
	if err != nil {
		if hasOperationalFailure(err) {
			return nil, document.EmailDisplayFailed, diagnostics, err
		}
		if policy, ok := errors.AsType[*policyLimitError](err); ok {
			diagnostics = append(diagnostics, d.diagnostic(policy.code, policy.operation, partPath, nil, "body display exceeds its declared byte limit")...)
			return nil, document.EmailDisplayTooLarge, diagnostics, nil
		}
		diagnostics = append(diagnostics, d.diagnostic(document.EmailDiagnosticCharsetInvalid, document.EmailOperationCharset, partPath, nil, "charset conversion failed")...)
		return nil, document.EmailDisplayFailed, diagnostics, nil
	}
	if validateUTF8 || validateASCII {
		invalidUTF8, nonASCII, validationErr := d.validateBodyArtifact(d.artifactFilename(partPath, document.EmailArtifactBodyUTF8))
		if validationErr != nil {
			return nil, document.EmailDisplayFailed, diagnostics, errors.Join(validationErr, d.discardLastBodyArtifact(ref))
		}
		if invalidUTF8 || validateASCII && nonASCII {
			if discardErr := d.discardLastBodyArtifact(ref); discardErr != nil {
				return nil, document.EmailDisplayFailed, diagnostics, discardErr
			}
			detail := "declared UTF-8 body is invalid"
			state := document.EmailDisplayFailed
			if charsetName == "" {
				detail = "undeclared text bytes are not valid UTF-8"
				state = document.EmailDisplayUnsupported
			} else if validateASCII {
				detail = "declared US-ASCII body contains a non-ASCII byte"
			}
			diagnostics = append(diagnostics, d.diagnostic(document.EmailDiagnosticCharsetInvalid, document.EmailOperationCharset, partPath, nil, detail)...)
			return nil, state, diagnostics, nil
		}
	}
	if charsetName != "" {
		replacement, replacementErr := d.artifactContainsReplacement(d.artifactFilename(partPath, document.EmailArtifactBodyUTF8))
		if replacementErr != nil {
			return nil, document.EmailDisplayFailed, diagnostics, replacementErr
		}
		if replacement {
			diagnostics = append(diagnostics, d.diagnostic(document.EmailDiagnosticCharsetReplacement, document.EmailOperationCharset, partPath, nil, "charset conversion emitted a replacement rune")...)
		}
	}
	return ref, document.EmailDisplayAvailable, diagnostics, nil
}

func (d *decoder) discardLastBodyArtifact(ref *document.EmailArtifactRefV1) error {
	last := len(d.artifacts) - 1
	if last < 0 || d.artifacts[last].artifact.Reference != *ref {
		return errors.New("email body artifact ownership changed")
	}
	if err := d.spool.remove(d.artifactFilename(d.artifacts[last].artifact.PartPath, document.EmailArtifactBodyUTF8)); err != nil {
		return &spoolIOError{err: err}
	}
	d.artifacts = d.artifacts[:last]
	d.bodyUTF8Bytes -= ref.Size
	return nil
}

func (d *decoder) validateBodyArtifact(name string) (invalidUTF8, nonASCII bool, resultErr error) {
	file, err := d.spool.openRegular(name)
	if err != nil {
		return false, false, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, &spoolIOError{err: closeErr})
		}
	}()
	reader := bufio.NewReader(file)
	for {
		value, size, readErr := reader.ReadRune()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return invalidUTF8, nonASCII, nil
			}
			return false, false, fmt.Errorf("validate UTF-8 body artifact: %w", readErr)
		}
		if value == utf8.RuneError && size == 1 {
			invalidUTF8 = true
		}
		if value >= utf8.RuneSelf {
			nonASCII = true
		}
	}
}

func (d *decoder) artifactContainsReplacement(name string) (result bool, resultErr error) {
	file, err := d.spool.openRegular(name)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, &spoolIOError{err: closeErr})
		}
	}()
	reader := bufio.NewReader(file)
	for {
		value, size, readErr := reader.ReadRune()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return false, nil
			}
			return false, fmt.Errorf("read UTF-8 body artifact: %w", readErr)
		}
		if value == utf8.RuneError && size == 3 {
			return true, nil
		}
	}
}
