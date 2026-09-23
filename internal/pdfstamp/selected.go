package pdfstamp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// SourceOutputPage records one page in output order. Both numbers are 1-based.
type SourceOutputPage struct {
	SourcePage int `json:"source_page"`
	OutputPage int `json:"output_page"`
}

// SelectedResult describes the exact bytes written by SelectPages or StampSelected.
type SelectedResult struct {
	SHA256    string             `json:"sha256"`
	Size      int64              `json:"size"`
	PageCount int                `json:"page_count"`
	PageMap   []SourceOutputPage `json:"page_map"`
}

// SelectPages collects exact 1-based source pages in caller order. The caller
// must run this bounded PDF operation in a supervised worker process. No bytes
// reach output until the collected PDF's page count has been verified.
func SelectPages(ctx context.Context, source io.ReadSeeker, pages []int, output io.Writer) (SelectedResult, error) {
	var zero SelectedResult
	if ctx == nil {
		return zero, stampFailure("validate context", errors.New("nil context"))
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if source == nil || output == nil {
		return zero, stampFailure("validate streams", errors.New("nil source or destination"))
	}
	if len(pages) == 0 {
		return zero, stampFailure("validate page selection", errors.New("at least one source page is required"))
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return zero, stampFailure("rewind source", err)
	}
	pageCount, err := api.PageCount(source, stampConfiguration())
	if err != nil {
		return zero, stampFailure("read source page count", err)
	}
	selection := make([]string, len(pages))
	pageMap := make([]SourceOutputPage, len(pages))
	seen := make(map[int]bool, len(pages))
	for index, page := range pages {
		if page < 1 || page > pageCount || seen[page] {
			return zero, stampFailure("validate page selection", fmt.Errorf("invalid or repeated source page %d", page))
		}
		seen[page] = true
		selection[index] = strconv.Itoa(page)
		pageMap[index] = SourceOutputPage{SourcePage: page, OutputPage: index + 1}
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return zero, stampFailure("rewind source", err)
	}
	var staged bytes.Buffer
	bounded := &limitedStampWriter{Writer: &staged, Remaining: MaxOutputBytes}
	if err := api.Collect(source, bounded, selection, stampConfiguration()); err != nil {
		return zero, stampFailure("collect selected pages", err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	verifiedCount, err := api.PageCount(bytes.NewReader(staged.Bytes()), stampConfiguration())
	if err != nil {
		return zero, stampFailure("verify selected page count", err)
	}
	if verifiedCount != len(pages) {
		return zero, stampFailure("verify selected page count", fmt.Errorf("got %d pages, want %d", verifiedCount, len(pages)))
	}
	if err := writeSelectedOutput(output, staged.Bytes()); err != nil {
		return zero, err
	}
	digest := sha256.Sum256(staged.Bytes())
	return SelectedResult{SHA256: hex.EncodeToString(digest[:]), Size: int64(staged.Len()), PageCount: verifiedCount, PageMap: pageMap}, nil
}

// StampSelected applies consecutive Bates labels to selected pages, preserving
// the source-page mapping while Stamp checks the final PDF and its labels.
func StampSelected(ctx context.Context, source io.ReadSeeker, labels []PageLabel, recipe Recipe, output io.Writer) (SelectedResult, error) {
	var zero SelectedResult
	if output == nil {
		return zero, stampFailure("validate streams", errors.New("nil destination"))
	}
	pages := make([]int, len(labels))
	for index, label := range labels {
		pages[index] = label.SourcePage
	}
	var selected bytes.Buffer
	selection, err := SelectPages(ctx, source, pages, &selected)
	if err != nil {
		return zero, err
	}
	outputLabels := make([]PageLabel, len(labels))
	for index, label := range labels {
		outputLabels[index] = PageLabel{SourcePage: index + 1, Label: label.Label}
	}
	result, err := Stamp(ctx, bytes.NewReader(selected.Bytes()), outputLabels, recipe, output)
	if err != nil {
		return zero, err
	}
	return SelectedResult{SHA256: result.SHA256, Size: result.Size, PageCount: result.PageCount, PageMap: selection.PageMap}, nil
}

// CombineStamped merges independently stamped source groups in order and
// verifies the combined page count and exact visible Bates labels before any
// bytes reach output.
func CombineStamped(ctx context.Context, groups [][]byte, labels []PageLabel, output io.Writer) (Result, error) {
	var zero Result
	if ctx == nil || output == nil || len(groups) == 0 || len(labels) == 0 {
		return zero, stampFailure("validate combined output", errors.New("groups, labels, context, and destination are required"))
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	readers := make([]io.ReadSeeker, len(groups))
	for index, group := range groups {
		if len(group) == 0 {
			return zero, stampFailure("validate combined output", errors.New("empty stamped group"))
		}
		readers[index] = bytes.NewReader(group)
	}
	var staged bytes.Buffer
	bounded := &limitedStampWriter{Writer: &staged, Remaining: MaxOutputBytes}
	if err := api.MergeRaw(readers, bounded, false, stampConfiguration()); err != nil {
		return zero, stampFailure("merge stamped PDFs", err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	pageCount, err := api.PageCount(bytes.NewReader(staged.Bytes()), stampConfiguration())
	if err != nil || pageCount != len(labels) {
		return zero, stampFailure("verify combined page count", errors.Join(err, fmt.Errorf("got %d pages, want %d", pageCount, len(labels))))
	}
	outputLabels := make([]PageLabel, len(labels))
	for index, label := range labels {
		outputLabels[index] = PageLabel{SourcePage: index + 1, Label: label.Label}
	}
	if err := verifyStampedLabels(staged.Bytes(), outputLabels); err != nil {
		return zero, err
	}
	if err := writeSelectedOutput(output, staged.Bytes()); err != nil {
		return zero, err
	}
	digest := sha256.Sum256(staged.Bytes())
	return Result{SHA256: hex.EncodeToString(digest[:]), Size: int64(staged.Len()), PageCount: pageCount, Pages: slices.Clone(labels)}, nil
}

// VerifyStamped independently parses retained output and checks its exact
// page count and visible label receipts.
func VerifyStamped(ctx context.Context, data []byte, labels []PageLabel) (Result, error) {
	if ctx == nil || len(data) == 0 || len(labels) == 0 {
		return Result{}, stampFailure("verify retained Bates PDF", errors.New("PDF bytes, labels, and context are required"))
	}
	return VerifyStampedReader(ctx, bytes.NewReader(data), labels)
}

// VerifyStampedReader independently parses a seekable retained output without
// requiring the complete PDF to be held in memory.
func VerifyStampedReader(ctx context.Context, source io.ReadSeeker, labels []PageLabel) (Result, error) {
	var zero Result
	if ctx == nil || source == nil || len(labels) == 0 {
		return zero, stampFailure("verify retained Bates PDF", errors.New("PDF stream, labels, and context are required"))
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return zero, stampFailure("rewind retained Bates PDF", err)
	}
	pageCount, err := api.PageCount(source, stampConfiguration())
	if err != nil || pageCount != len(labels) {
		return zero, stampFailure("verify retained page count", errors.Join(err, fmt.Errorf("got %d pages, want %d", pageCount, len(labels))))
	}
	outputLabels := make([]PageLabel, len(labels))
	for index, label := range labels {
		outputLabels[index] = PageLabel{SourcePage: index + 1, Label: label.Label}
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return zero, stampFailure("rewind retained Bates PDF", err)
	}
	if err := verifyStampedLabelsReader(source, outputLabels); err != nil {
		return zero, err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return zero, stampFailure("rewind retained Bates PDF", err)
	}
	digest := sha256.New()
	size, err := io.Copy(digest, source)
	if err != nil || size < 1 {
		return zero, stampFailure("hash retained Bates PDF", err)
	}
	return Result{SHA256: hex.EncodeToString(digest.Sum(nil)), Size: size, PageCount: pageCount, Pages: slices.Clone(labels)}, nil
}

func writeSelectedOutput(output io.Writer, data []byte) error {
	written, err := output.Write(data)
	if err != nil {
		return stampFailure("write verified output", err)
	}
	if written != len(data) {
		return stampFailure("write verified output", io.ErrShortWrite)
	}
	return nil
}
