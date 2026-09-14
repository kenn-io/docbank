package emailmime

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

type Artifact struct {
	PartPath  string
	Reference document.EmailArtifactRefV1
}

type Result struct {
	Evidence  document.EmailV1
	mu        sync.Mutex
	spool     *ownedSpool
	artifacts []storedArtifact
	closed    bool
}

type decoder struct {
	ctx                context.Context
	limits             document.EmailLimitsV1
	spool              *ownedSpool
	artifacts          []storedArtifact
	parts              []document.EmailPartV1
	messages           []document.EmailMessageV1
	messageIndex       map[string]int
	headerBytes        int64
	headerFields       int
	decodedBytes       int64
	bodyUTF8Bytes      int64
	headerDisplayBytes int64
	diagnosticCount    int
	termination        *document.EmailTerminationV1
}

func Recipe() document.EmailRecipeV1 {
	return recipeWithLimits(document.EmailLimitsV1{SourceBytes: 128 << 20, PartBytes: 128 << 20, DecodedBytes: 256 << 20, Parts: 1000, Depth: 16, HeaderBytes: 1 << 20, AggregateHeaderBytes: 8 << 20, HeaderFields: 4096, AggregateHeaderFields: 16384, BodyUTF8Bytes: 16 << 20, AggregateBodyUTF8Bytes: 256 << 20, HTMLDisplayBytes: 16 << 20, InventoryBytes: 8 << 20, Diagnostics: 4096, DiagnosticDetailBytes: 1024, HeaderDisplayBytes: 1 << 20})
}

func recipeWithLimits(limits document.EmailLimitsV1) document.EmailRecipeV1 {
	return document.EmailRecipeV1{ContractVersion: document.EmailRecipeContractV1, ImplementationRevision: 3, GoVersion: runtime.Version(), CharsetProfile: "docbank-email-charset/v1", HeaderProfile: "docbank-email-header/v1", FilenameProfile: "docbank-email-filename/v1", BodyProfile: "docbank-email-body-selection/v1", Limits: limits}
}

func Decode(ctx context.Context, sourceSHA256 string, sourceSize int64, source io.Reader, spoolParent string) (*Result, error) {
	return decodeWithLimits(ctx, sourceSHA256, sourceSize, source, spoolParent, Recipe().Limits)
}

func decodeWithLimits(ctx context.Context, sourceSHA256 string, sourceSize int64, source io.Reader, spoolParent string, limits document.EmailLimitsV1) (result *Result, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if len(sourceSHA256) != 64 || strings.ToLower(sourceSHA256) != sourceSHA256 {
		return nil, errors.New("email source SHA-256 is invalid")
	}
	if _, err = hex.DecodeString(sourceSHA256); err != nil {
		return nil, errors.New("email source SHA-256 is invalid")
	}
	if sourceSize < 0 {
		return nil, errors.New("email source size is invalid")
	}
	if source == nil {
		return nil, errors.New("email source reader is nil")
	}
	recipe := recipeWithLimits(limits)
	if sourceSize > limits.SourceBytes {
		limit := limits.SourceBytes
		observed := sourceSize
		return &Result{Evidence: document.EmailV1{ContractVersion: document.EmailContractV1, Source: document.EmailSourceV1{SHA256: sourceSHA256, Size: sourceSize, Verification: document.EmailVerificationCatalogOnly}, Recipe: recipe, Outcome: document.EmailOutcomeUnavailable, Failure: &document.EmailFailureV1{Code: document.EmailDiagnosticSourceSizeLimit, Operation: document.EmailOperationSource, Limit: &limit, Observed: &observed, Detail: "catalog source size exceeds the email decoder limit"}}}, nil
	}
	spool, err := createSpool(spoolParent)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			cleanupErr := spool.cleanup()
			if cleanupErr != nil {
				result = nil
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	if err = copyVerifiedSource(ctx, source, spool, "source.eml", sourceSHA256, sourceSize, limits.SourceBytes); err != nil {
		return nil, err
	}
	d := &decoder{ctx: ctx, limits: limits, spool: spool, artifacts: []storedArtifact{}, parts: []document.EmailPartV1{}, messages: []document.EmailMessageV1{}, messageIndex: make(map[string]int)}
	file, err := spool.openRegular("source.eml")
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(file, multipartPeekBufferSize)
	err = d.parseEntity("1", nil, 1, 1, "1", reader, nil)
	closeErr := file.Close()
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	d.finishMessages()
	root := "1"
	inventory := &document.EmailInventoryV1{State: document.EmailInventoryComplete, RootPath: &root, Parts: d.parts, Messages: d.messages, Diagnostics: []document.EmailDiagnosticV1{}, Termination: nil}
	if d.termination != nil {
		inventory.State = document.EmailInventoryPartial
		inventory.Termination = d.termination
		if len(d.parts) == 0 {
			inventory.RootPath = nil
		}
	}
	evidence := document.EmailV1{ContractVersion: document.EmailContractV1, Source: document.EmailSourceV1{SHA256: sourceSHA256, Size: sourceSize, Verification: document.EmailVerificationVerified}, Recipe: recipe, Outcome: document.EmailOutcomeDecoded, Inventory: inventory}
	observed, inventoryExceeded, sizeErr := canonical.BoundedSize(inventory, limits.InventoryBytes)
	if sizeErr != nil {
		return nil, fmt.Errorf("measure email inventory: %w", sizeErr)
	}
	if inventoryExceeded {
		limit := limits.InventoryBytes
		evidence = document.EmailV1{ContractVersion: document.EmailContractV1, Source: evidence.Source, Recipe: recipe, Outcome: document.EmailOutcomeUnavailable, Failure: &document.EmailFailureV1{Code: document.EmailDiagnosticInventoryMetadataLimit, Operation: document.EmailOperationInventory, Limit: &limit, Observed: &observed, Detail: "canonical email inventory exceeds its metadata limit"}}
		if removeErr := spool.cleanup(); removeErr != nil {
			return nil, removeErr
		}
		keep = true
		return &Result{Evidence: evidence}, nil
	}
	if _, _, marshalErr := document.MarshalEmailV1(evidence); marshalErr != nil {
		return nil, marshalErr
	}
	result = &Result{Evidence: evidence, spool: spool, artifacts: d.artifacts}
	keep = true
	return result, nil
}

func copyVerifiedSource(ctx context.Context, source io.Reader, spool *ownedSpool, name, wantSHA string, wantSize, limit int64) (resultErr error) {
	file, err := spool.create(name)
	if err != nil {
		return err
	}
	ok := false
	closed := false
	defer func() {
		var closeErr error
		if !closed {
			closeErr = file.Close()
		}
		var removeErr error
		if !ok {
			removeErr = spool.remove(name)
		}
		if closeErr != nil || removeErr != nil {
			resultErr = errors.Join(resultErr, &spoolIOError{err: errors.Join(closeErr, removeErr)})
		}
	}()
	h := sha256.New()
	buffer := make([]byte, 32<<10)
	var size int64
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		readSize := len(buffer)
		remaining := limit - size
		if remaining < int64(readSize) {
			readSize = int(remaining) + 1
		}
		if readSize < 1 {
			readSize = 1
		}
		n, readErr := source.Read(buffer[:readSize])
		if n > 0 {
			if size+int64(n) > limit {
				return errors.New("email source exceeded its declared byte limit")
			}
			written, writeErr := file.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			_, _ = h.Write(buffer[:n])
			size += int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	if size != wantSize {
		return fmt.Errorf("email source size mismatch: expected %d bytes, read %d", wantSize, size)
	}
	if hex.EncodeToString(h.Sum(nil)) != wantSHA {
		return errors.New("email source SHA-256 mismatch")
	}
	if err = file.Close(); err != nil {
		return &spoolIOError{err: err}
	}
	closed = true
	ok = true
	return nil
}

func (d *decoder) parseEntity(path string, parent *string, sibling, depth int, messagePath string, body io.Reader, provided *parsedHeaderBlock) error {
	if d.termination != nil {
		return nil
	}
	if err := d.ctx.Err(); err != nil {
		return err
	}
	if len(d.parts) >= d.limits.Parts {
		d.stop(document.EmailDiagnosticPartCountLimit, document.EmailOperationStructure, path, int64(d.limits.Parts), int64(len(d.parts)+1))
		return nil
	}
	if depth > d.limits.Depth {
		d.stop(document.EmailDiagnosticDepthLimit, document.EmailOperationStructure, path, int64(d.limits.Depth), int64(depth))
		return nil
	}
	var block parsedHeaderBlock
	var err error
	if provided != nil {
		block = *provided
	} else {
		buffered, ok := body.(*bufio.Reader)
		if !ok {
			buffered = bufio.NewReaderSize(body, multipartPeekBufferSize)
			body = buffered
		}
		block, err = readHeaderBlock(d.ctx, buffered, d.limits.HeaderBytes, d.limits.HeaderFields, &d.headerBytes, d.limits.AggregateHeaderBytes, &d.headerFields, d.limits.AggregateHeaderFields)
		if err != nil {
			if policy := asPolicyLimit(err, path); policy != nil {
				return d.stopPolicy(policy, path)
			}
			return err
		}
	}
	headerRef, err := d.storeBytes(path, document.EmailArtifactRawHeaders, block.raw)
	if err != nil {
		return err
	}
	part := document.EmailPartV1{Path: path, ParentPath: cloneString(parent), SiblingOrder: sibling, Depth: depth, MessagePath: messagePath, Media: document.EmailMediaV1{Diagnostics: []document.EmailDiagnosticV1{}}, HeaderBlock: headerRef, Headers: emailHeaders(block), Filename: document.EmailFilenameV1{Fields: []int{}, State: document.EmailInterpretationMissing}, ContentID: document.EmailContentIDV1{Fields: []int{}, State: document.EmailInterpretationMissing}, DecodeState: document.EmailDecodeFailed, Protection: document.EmailProtectionNone, Diagnostics: []document.EmailDiagnosticV1{}}
	for _, field := range block.fields {
		if !field.valid {
			part.Diagnostics = append(part.Diagnostics, d.diagnosticAt(document.EmailDiagnosticMalformedHeader, document.EmailOperationHeaders, path, field.index, "header field is malformed")...)
		}
	}
	mediaType, params := d.interpretMedia(path, block, &part)
	filename, filenameDiagnostics := d.interpretFilename(path, block)
	part.Filename = filename
	part.Diagnostics = append(part.Diagnostics, filenameDiagnostics...)
	contentID, contentIDDiagnostics := d.interpretContentID(path, block)
	part.ContentID = contentID
	part.Diagnostics = append(part.Diagnostics, contentIDDiagnostics...)
	if path == messagePath {
		d.messageIndex[path] = len(d.messages)
		d.messages = append(d.messages, d.interpretMessage(path, block))
	}
	d.parts = append(d.parts, part)
	partIndex := len(d.parts) - 1
	switch mediaType {
	case "multipart/signed":
		d.parts[partIndex].Protection = document.EmailProtectionSignedUnverified
		d.parts[partIndex].Diagnostics = append(d.parts[partIndex].Diagnostics, d.diagnostic(document.EmailDiagnosticSignatureUnverified, document.EmailOperationStructure, path, nil, "signed MIME is not cryptographically verified")...)
	case "multipart/encrypted", "application/pkcs7-mime", "application/x-pkcs7-mime":
		d.parts[partIndex].Protection = document.EmailProtectionEncrypted
		d.parts[partIndex].Diagnostics = append(d.parts[partIndex].Diagnostics, d.diagnostic(document.EmailDiagnosticEncryptedUnavailable, document.EmailOperationStructure, path, nil, "encrypted MIME plaintext is unavailable")...)
	}
	transfer := ""
	if fields := headerValues(block, "content-transfer-encoding"); len(fields) > 0 {
		transfer = strings.ToLower(strings.TrimSpace(fields[0].value))
		if transfer == "" {
			d.parts[partIndex].Diagnostics = append(d.parts[partIndex].Diagnostics, d.diagnosticAt(document.EmailDiagnosticInvalidHeader, document.EmailOperationHeaders, path, fields[0].index, "Content-Transfer-Encoding header is empty")...)
		} else {
			d.parts[partIndex].TransferEncoding = &transfer
		}
		if len(fields) > 1 {
			d.parts[partIndex].Diagnostics = append(d.parts[partIndex].Diagnostics, d.diagnostic(document.EmailDiagnosticDuplicateHeader, document.EmailOperationHeaders, path, nil, "multiple Content-Transfer-Encoding fields")...)
		}
	}
	decodedReader, transferErr := transferDecodedReader(body, transfer)
	if transferErr != nil {
		d.parts[partIndex].DecodeState = document.EmailDecodeUnsupported
		d.parts[partIndex].Diagnostics = append(d.parts[partIndex].Diagnostics, d.diagnostic(document.EmailDiagnosticTransferUnsupported, document.EmailOperationTransfer, path, nil, "Content-Transfer-Encoding is unsupported")...)
		drainErr := drainPart(body)
		if errors.Is(drainErr, errBoundaryUnclosed) {
			d.stop(document.EmailDiagnosticBoundaryUnclosed, document.EmailOperationStructure, path, 0, 0)
		} else if drainErr != nil {
			return drainErr
		}
		if strings.HasPrefix(mediaType, "multipart/") || mediaType == "message/rfc822" {
			d.stop(document.EmailDiagnosticTransferUnsupported, document.EmailOperationTransfer, path, 0, 0)
		}
		d.addUnavailableAlternative(partIndex, mediaType, document.EmailDisplayUnsupported)
		return nil
	}
	payloadRef, err := d.storeStream(path, document.EmailArtifactDecodedPayload, decodedReader, d.limits.PartBytes, &d.decodedBytes)
	if err != nil {
		if hasOperationalFailure(err) {
			return err
		}
		if errors.Is(err, errBoundaryUnclosed) {
			d.addUnavailableAlternative(partIndex, mediaType, document.EmailDisplayFailed)
			d.stop(document.EmailDiagnosticBoundaryUnclosed, document.EmailOperationStructure, path, 0, 0)
			return nil
		}
		if policy := asPolicyLimit(err, path); policy != nil {
			d.addUnavailableAlternative(partIndex, mediaType, document.EmailDisplayFailed)
			return d.stopPolicy(policy, path)
		}
		if !malformedTransferError(err, transfer) {
			return err
		}
		d.parts[partIndex].DecodeState = document.EmailDecodeFailed
		d.parts[partIndex].Diagnostics = append(d.parts[partIndex].Diagnostics, d.diagnostic(document.EmailDiagnosticTransferInvalid, document.EmailOperationTransfer, path, nil, "transfer encoding is malformed")...)
		drainErr := drainPart(body)
		if errors.Is(drainErr, errBoundaryUnclosed) {
			d.stop(document.EmailDiagnosticBoundaryUnclosed, document.EmailOperationStructure, path, 0, 0)
		} else if drainErr != nil {
			return drainErr
		}
		if strings.HasPrefix(mediaType, "multipart/") || mediaType == "message/rfc822" {
			d.stop(document.EmailDiagnosticTransferInvalid, document.EmailOperationTransfer, path, 0, 0)
		}
		d.addUnavailableAlternative(partIndex, mediaType, document.EmailDisplayFailed)
		return nil
	}
	d.parts[partIndex].DecodeState = document.EmailDecodeDecoded
	d.parts[partIndex].Payload = payloadRef
	payloadName := d.artifactFilename(path, document.EmailArtifactDecodedPayload)
	detected, detectErr := d.detectPayloadMedia(payloadName)
	if detectErr != nil {
		return detectErr
	}
	d.parts[partIndex].Media.Detected = &detected
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			d.parts[partIndex].Diagnostics = append(d.parts[partIndex].Diagnostics, d.diagnostic(document.EmailDiagnosticBoundaryMissing, document.EmailOperationStructure, path, nil, "multipart boundary is missing")...)
			d.stop(document.EmailDiagnosticBoundaryMissing, document.EmailOperationStructure, path, 0, 0)
			return nil
		}
		if len(boundary) > 70 || strings.ContainsAny(boundary, "\r\n") {
			d.stop(document.EmailDiagnosticBoundaryInvalid, document.EmailOperationStructure, path, 0, 0)
			return nil
		}
		if err = d.parseMultipart(path, depth, messagePath, payloadName, boundary); err != nil {
			return err
		}
		return nil
	}
	if mediaType == "message/rfc822" {
		nestedPath := path + ".1"
		parentPath := path
		file, openErr := d.spool.openRegular(payloadName)
		if openErr != nil {
			return openErr
		}
		nestedErr := d.parseEntity(nestedPath, &parentPath, 1, depth+1, nestedPath, bufio.NewReaderSize(file, multipartPeekBufferSize), nil)
		closeErr := file.Close()
		return errors.Join(nestedErr, closeErr)
	}
	if mediaType == "application/pkcs7-mime" || mediaType == "application/x-pkcs7-mime" {
		return nil
	}
	if (mediaType == "text/plain" || mediaType == "text/html") && d.bodyEligible(partIndex) {
		bodyRef, state, diagnostics, bodyErr := d.deriveBody(path, mediaType, params["charset"], payloadName)
		if bodyErr != nil {
			return bodyErr
		}
		d.parts[partIndex].BodyUTF8 = bodyRef
		d.parts[partIndex].Diagnostics = append(d.parts[partIndex].Diagnostics, diagnostics...)
		kind := document.EmailBodyPlain
		if mediaType == "text/html" {
			kind = document.EmailBodyHTML
		}
		message := &d.messages[d.messageIndex[messagePath]]
		message.Alternatives = append(message.Alternatives, document.EmailAlternativeV1{PartPath: path, Kind: kind, DisplayState: state, Display: bodyRef, Diagnostics: []document.EmailDiagnosticV1{}})
	}
	return nil
}

func (d *decoder) addUnavailableAlternative(partIndex int, mediaType string, state document.EmailDisplayState) {
	if mediaType != "text/plain" && mediaType != "text/html" || !d.bodyEligible(partIndex) {
		return
	}
	kind := document.EmailBodyPlain
	if mediaType == "text/html" {
		kind = document.EmailBodyHTML
	}
	part := d.parts[partIndex]
	message := &d.messages[d.messageIndex[part.MessagePath]]
	message.Alternatives = append(message.Alternatives, document.EmailAlternativeV1{PartPath: part.Path, Kind: kind, DisplayState: state, Display: nil, Diagnostics: []document.EmailDiagnosticV1{}})
}

func (d *decoder) bodyEligible(partIndex int) bool {
	part := d.parts[partIndex]
	messagePath := part.MessagePath
	if part.IsAttachmentLike() {
		return false
	}
	for part.ParentPath != nil {
		parentIndex := d.partIndex(*part.ParentPath)
		if parentIndex < 0 {
			return false
		}
		parent := d.parts[parentIndex]
		if parent.MessagePath != messagePath {
			break
		}
		if parent.IsAttachmentLike() || parent.Protection == document.EmailProtectionEncrypted {
			return false
		}
		part = parent
	}
	return true
}

func (d *decoder) partIndex(path string) int {
	for index := range slices.Backward(d.parts) {
		if d.parts[index].Path == path {
			return index
		}
	}
	return -1
}

func (d *decoder) parseMultipart(parentPath string, depth int, messagePath, payloadFile, boundary string) (resultErr error) {
	file, err := d.spool.openRegular(payloadFile)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, &spoolIOError{err: closeErr})
		}
	}()
	input := &multipartInput{ctx: d.ctx, reader: bufio.NewReader(io.LimitReader(infrastructureMarkingReader{reader: file}, d.limits.PartBytes+1)), boundary: "--" + boundary, lineStart: true}
	reader := multipart.NewReader(input, boundary)
	var lastBodyStart int64
	for next := 1; ; next++ {
		currentPath := parentPath + "." + strconv.Itoa(next)
		part, nextErr := reader.NextRawPart()
		// The standard parser also returns EOF from an unfinished header.
		// A new opening delimiter distinguishes that refusal from the final boundary.
		if nextErr == io.EOF && input.headerStart <= lastBodyStart {
			return nil
		}
		if nextErr != nil {
			if hasOperationalFailure(nextErr) {
				return fmt.Errorf("read email MIME part: %w", nextErr)
			}
			code := document.EmailDiagnosticBoundaryInvalid
			if errors.Is(nextErr, io.EOF) || errors.Is(nextErr, io.ErrUnexpectedEOF) {
				code = document.EmailDiagnosticBoundaryUnclosed
			}
			d.stop(code, document.EmailOperationStructure, currentPath, 0, 0)
			return nil
		}
		// NextRawPart has consumed exactly the header block: multipartInput
		// ends every read at a line boundary so it cannot read ahead into the body.
		lastBodyStart = input.offset
		headers := bufio.NewReader(io.NewSectionReader(file, input.headerStart, input.offset-input.headerStart))
		block, headerErr := readHeaderBlock(d.ctx, headers, d.limits.HeaderBytes, d.limits.HeaderFields, &d.headerBytes, d.limits.AggregateHeaderBytes, &d.headerFields, d.limits.AggregateHeaderFields)
		if policy := asPolicyLimit(headerErr, currentPath); policy != nil {
			return d.stopPolicy(policy, currentPath)
		}
		if headerErr != nil {
			return headerErr
		}
		if err = d.parseEntity(currentPath, &parentPath, next, depth+1, messagePath, multipartPayload{part}, &block); err != nil {
			return err
		}
		if d.termination != nil {
			return nil
		}
	}
}

func (d *decoder) interpretMedia(path string, block parsedHeaderBlock, part *document.EmailPartV1) (string, map[string]string) {
	mediaType := "text/plain"
	params := map[string]string{}
	fields := headerValues(block, "content-type")
	if len(fields) > 0 {
		parsed, parsedParams, err := mime.ParseMediaType(fields[0].value)
		// ParseMediaType also accepts bare tokens for Content-Disposition.
		if err != nil || !strings.Contains(parsed, "/") {
			part.Media.Diagnostics = append(part.Media.Diagnostics, d.essentialDiagnosticAt(&part.Diagnostics, document.EmailDiagnosticInvalidHeader, document.EmailOperationHeaders, path, fields[0].index, "Content-Type header is invalid")...)
			mediaType = "application/octet-stream"
		} else {
			mediaType = strings.ToLower(parsed)
			declared := mediaType
			part.Media.Declared = &declared
			params = parsedParams
		}
		if len(fields) > 1 {
			part.Media.Diagnostics = append(part.Media.Diagnostics, d.diagnostic(document.EmailDiagnosticDuplicateHeader, document.EmailOperationHeaders, path, nil, "multiple Content-Type fields")...)
		}
	}
	dispositionFields := headerValues(block, "content-disposition")
	if len(dispositionFields) > 0 {
		value, _, err := mime.ParseMediaType(dispositionFields[0].value)
		if err != nil {
			part.Diagnostics = append(part.Diagnostics, d.diagnosticAt(document.EmailDiagnosticInvalidHeader, document.EmailOperationHeaders, path, dispositionFields[0].index, "Content-Disposition header is invalid")...)
		} else {
			value = strings.ToLower(value)
			part.Disposition = &value
		}
	}
	return mediaType, params
}

func (d *decoder) essentialDiagnosticAt(optional *[]document.EmailDiagnosticV1, code document.EmailDiagnosticCode, operation document.EmailOperation, path string, index int, detail string) []document.EmailDiagnosticV1 {
	if d.diagnosticCount >= d.limits.Diagnostics {
		observed := d.diagnosticCount + 1
		if removed := d.evictOptionalDiagnostic(optional); removed > 0 {
			d.stop(document.EmailDiagnosticLimit, document.EmailOperationInventory, path, int64(d.limits.Diagnostics), int64(observed))
			d.diagnosticCount -= removed
		}
	}
	return d.diagnosticAt(code, operation, path, index, detail)
}

func (d *decoder) evictOptionalDiagnostic(current *[]document.EmailDiagnosticV1) int {
	if popOptionalDiagnostic(current) {
		return 1
	}
	for index := len(d.parts) - 1; index >= 0; index-- {
		if popOptionalDiagnostic(&d.parts[index].Diagnostics) || popOptionalDiagnostic(&d.parts[index].Media.Diagnostics) {
			return 1
		}
	}
	for index := len(d.messages) - 1; index >= 0; index-- {
		if popOptionalMessageDiagnostic(&d.messages[index].Diagnostics) {
			return 1
		}
		for alternative := len(d.messages[index].Alternatives) - 1; alternative >= 0; alternative-- {
			if popOptionalDiagnostic(&d.messages[index].Alternatives[alternative].Diagnostics) {
				return 1
			}
		}
	}
	for index := len(d.messages) - 1; index >= 0; index-- {
		if removed := popOptionalDateDiagnostic(&d.messages[index].Date); removed > 0 {
			return removed
		}
	}
	return 0
}

func popOptionalDateDiagnostic(date *document.EmailDateV1) int {
	if popOptionalDiagnostic(&date.Diagnostics) {
		return 1
	}
	for index := range slices.Backward(date.Diagnostics) {
		switch date.Diagnostics[index].Code {
		case document.EmailDiagnosticTimezoneUnknown, document.EmailDiagnosticLeapSecondInstantUnavailable:
			removeDateDiagnosticAt(date, index)
			return 1 + degradeEmailDate(date)
		default:
			continue
		}
	}
	return 0
}

func popOptionalMessageDiagnostic(diagnostics *[]document.EmailDiagnosticV1) bool {
	if popOptionalDiagnostic(diagnostics) {
		return true
	}
	for index := range slices.Backward(*diagnostics) {
		if (*diagnostics)[index].Code != document.EmailDiagnosticInvalidHeader {
			continue
		}
		copy((*diagnostics)[index:], (*diagnostics)[index+1:])
		*diagnostics = (*diagnostics)[:len(*diagnostics)-1]
		return true
	}
	return false
}

func popOptionalDiagnostic(diagnostics *[]document.EmailDiagnosticV1) bool {
	for index := range slices.Backward(*diagnostics) {
		switch (*diagnostics)[index].Code {
		case document.EmailDiagnosticMalformedHeader, document.EmailDiagnosticDuplicateHeader,
			document.EmailDiagnosticHeaderDisplayLimit, document.EmailDiagnosticEncodedWordInvalid,
			document.EmailDiagnosticFilenameInvalid, document.EmailDiagnosticFilenameUnsupported,
			document.EmailDiagnosticContentIDInvalid, document.EmailDiagnosticContentIDAmbiguous,
			document.EmailDiagnosticCharsetMissing, document.EmailDiagnosticCharsetUnsupported,
			document.EmailDiagnosticCharsetInvalid, document.EmailDiagnosticCharsetReplacement,
			document.EmailDiagnosticTransferUnsupported, document.EmailDiagnosticTransferInvalid,
			document.EmailDiagnosticBodyUTF8Limit, document.EmailDiagnosticBodyUTF8TotalLimit,
			document.EmailDiagnosticHTMLDisplayLimit, document.EmailDiagnosticBodyUnavailable,
			document.EmailDiagnosticSignatureUnverified, document.EmailDiagnosticEncryptedUnavailable,
			document.EmailDiagnosticDateMissing, document.EmailDiagnosticDateInvalid,
			document.EmailDiagnosticDateAmbiguous, document.EmailDiagnosticTimezoneOriginUnknown:
			copy((*diagnostics)[index:], (*diagnostics)[index+1:])
			*diagnostics = (*diagnostics)[:len(*diagnostics)-1]
			return true
		default:
			continue
		}
	}
	return false
}

func (d *decoder) artifactFilename(path string, role document.EmailArtifactRole) string {
	for _, artifact := range d.artifacts {
		if artifact.artifact.PartPath == path && artifact.artifact.Reference.Role == role {
			return artifact.filename
		}
	}
	return ""
}

func (d *decoder) detectPayloadMedia(name string) (detected string, resultErr error) {
	file, err := d.spool.openRegular(name)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, &spoolIOError{err: closeErr})
		}
	}()
	buffer := make([]byte, 512)
	n, err := io.ReadFull(file, buffer)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	detected, _, parseErr := mime.ParseMediaType(http.DetectContentType(buffer[:n]))
	if parseErr != nil {
		return "", fmt.Errorf("normalize detected payload media type: %w", parseErr)
	}
	return strings.ToLower(detected), nil
}
func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}
func asPolicyLimit(err error, path string) *policyLimitError {
	if policy, ok := errors.AsType[*policyLimitError](err); ok {
		result := *policy
		if result.path == "" {
			result.path = path
		}
		return &result
	}
	return nil
}

func (d *decoder) stopPolicy(policy *policyLimitError, path string) error {
	d.stop(policy.code, policy.operation, path, policy.limit, policy.observed)
	return nil
}
func (d *decoder) stop(code document.EmailDiagnosticCode, operation document.EmailOperation, path string, limit, observed int64) {
	if d.termination != nil {
		return
	}
	pathValue := path
	termination := &document.EmailTerminationV1{Code: code, Operation: operation, Path: &pathValue}
	if limit > 0 {
		termination.Limit = &limit
	}
	if observed > 0 {
		termination.Observed = &observed
	}
	d.termination = termination
}
func (d *decoder) diagnostic(code document.EmailDiagnosticCode, operation document.EmailOperation, path string, header *int, detail string) []document.EmailDiagnosticV1 {
	observed := d.diagnosticCount + 1
	if observed > d.limits.Diagnostics {
		d.stop(document.EmailDiagnosticLimit, document.EmailOperationInventory, path, int64(d.limits.Diagnostics), int64(observed))
		return []document.EmailDiagnosticV1{}
	}
	d.diagnosticCount = observed
	detail = truncateUTF8(detail, d.limits.DiagnosticDetailBytes)
	pathValue := path
	return []document.EmailDiagnosticV1{{Code: code, Operation: operation, Path: &pathValue, HeaderIndex: header, Detail: detail}}
}

func truncateUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}
func (d *decoder) diagnosticAt(code document.EmailDiagnosticCode, operation document.EmailOperation, path string, index int, detail string) []document.EmailDiagnosticV1 {
	return d.diagnostic(code, operation, path, &index, detail)
}

func (d *decoder) finishMessages() {
	// Every declared related container needs a group, including containers
	// whose boundary, transfer encoding, or budget prevented traversal.
	for _, part := range d.parts {
		if part.Media.Declared != nil && *part.Media.Declared == "multipart/related" {
			d.addRelatedGroup(part.MessagePath, part.Path)
		}
	}
	for index := range d.messages {
		message := &d.messages[index]
		var plain, html *string
		for _, alternative := range message.Alternatives {
			if alternative.DisplayState != document.EmailDisplayAvailable {
				continue
			}
			path := alternative.PartPath
			if alternative.Kind == document.EmailBodyHTML && html == nil {
				html = &path
			}
			if alternative.Kind == document.EmailBodyPlain && plain == nil {
				plain = &path
			}
		}
		if html != nil {
			message.SelectedBodyPath = html
		} else {
			message.SelectedBodyPath = plain
		}
		if message.SelectedBodyPath == nil && len(message.Alternatives) > 0 {
			message.Diagnostics = append(message.Diagnostics, d.diagnostic(document.EmailDiagnosticBodyUnavailable, document.EmailOperationBodySelection, message.Path, nil, "no message body alternative is available")...)
		}
	}
}

func (d *decoder) addRelatedGroup(messagePath, rootPath string) {
	message := &d.messages[d.messageIndex[messagePath]]
	group := document.EmailRelatedGroupV1{RootPath: rootPath, Resources: []document.EmailResourceV1{}}
	partsByPath := make(map[string]*document.EmailPartV1, len(d.parts))
	children := make(map[string]bool, len(d.parts))
	for index := range d.parts {
		part := &d.parts[index]
		partsByPath[part.Path] = part
		if part.ParentPath != nil {
			children[*part.ParentPath] = true
		}
	}
	alternatives := make(map[string]document.EmailAlternativeV1, len(message.Alternatives))
	var plainBodyPath *string
	for _, alternative := range message.Alternatives {
		alternatives[alternative.PartPath] = alternative
		if alternative.DisplayState != document.EmailDisplayAvailable || nearestRelatedRoot(alternative.PartPath, messagePath, partsByPath) != rootPath {
			continue
		}
		if alternative.Kind == document.EmailBodyHTML && group.BodyPath == nil {
			path := alternative.PartPath
			group.BodyPath = &path
		} else if alternative.Kind == document.EmailBodyPlain && plainBodyPath == nil {
			path := alternative.PartPath
			plainBodyPath = &path
		}
	}
	if group.BodyPath == nil {
		group.BodyPath = plainBodyPath
	}
	resourceIndex := make(map[string]int)
	missingAdded := false
	for index := range d.parts {
		part := &d.parts[index]
		if part.Path == rootPath || part.MessagePath != messagePath || nearestRelatedRoot(part.Path, messagePath, partsByPath) != rootPath {
			continue
		}
		if part.ContentID.State == document.EmailInterpretationDecoded && part.ContentID.Value != nil {
			cid := *part.ContentID.Value
			entryIndex, ok := resourceIndex[cid]
			if !ok {
				value := cid
				resourceIndex[cid] = len(group.Resources)
				group.Resources = append(group.Resources, document.EmailResourceV1{CID: &value, Candidates: []string{part.Path}, State: document.EmailResourceUnique})
			} else {
				resource := &group.Resources[entryIndex]
				resource.Candidates = append(resource.Candidates, part.Path)
				resource.State = document.EmailResourceAmbiguous
			}
			continue
		}
		_, bodyAlternative := alternatives[part.Path]
		declared := ""
		if part.Media.Declared != nil {
			declared = *part.Media.Declared
		}
		eligible := !children[part.Path] && !bodyAlternative && !strings.HasPrefix(declared, "multipart/") && declared != "message/rfc822" && (part.Disposition == nil || *part.Disposition != "attachment")
		if eligible {
			if !missingAdded {
				group.Resources = append(group.Resources, document.EmailResourceV1{CID: nil, Candidates: []string{}, State: document.EmailResourceMissing})
				missingAdded = true
			}
			part.Diagnostics = append(part.Diagnostics, d.diagnostic(document.EmailDiagnosticCIDMissing, document.EmailOperationCID, part.Path, nil, "inline related resource has no usable Content-ID")...)
		}
	}
	message.RelatedGroups = append(message.RelatedGroups, group)
}

func nearestRelatedRoot(path, messagePath string, parts map[string]*document.EmailPartV1) string {
	part := parts[path]
	for part != nil && part.ParentPath != nil {
		parent := parts[*part.ParentPath]
		if parent == nil || parent.MessagePath != messagePath {
			return ""
		}
		if parent.Media.Declared != nil && *parent.Media.Declared == "multipart/related" {
			return parent.Path
		}
		part = parent
	}
	return ""
}
