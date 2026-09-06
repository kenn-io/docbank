package csvpdf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/ocr"
)

type trackedReader struct {
	io.Reader

	closed   bool
	closeErr error
}

func (r *trackedReader) Close() error { r.closed = true; return r.closeErr }

func sourceFor(t *testing.T, content string) (ocr.Source, *trackedReader) {
	t.Helper()
	reader := &trackedReader{Reader: strings.NewReader(content)}
	source, err := ocr.NewSource(reader, "text/csv", int64(len(content)), digest([]byte(content)))
	require.NoError(t, err)
	return source, reader
}

func policyFor(t *testing.T, limits Limits) Policy {
	t.Helper()
	policy, err := NewPolicy(limits)
	require.NoError(t, err)
	return policy
}

func TestConvertDeterministicProvenanceAndCopies(t *testing.T) {
	content := "name,value,empty\r\n\"café κόσμος Привет\",\"one\ntwo\",\r\nshort\r\n"
	policy := policyFor(t, DefaultLimits())
	source, reader := sourceFor(t, content)
	result, err := Convert(t.Context(), source, policy)
	require.NoError(t, err)
	require.True(t, reader.closed)
	source, _ = sourceFor(t, content)
	again, err := Convert(t.Context(), source, policy)
	require.NoError(t, err)
	require.Equal(t, result.PDF(), again.PDF())
	receipt := result.Receipt()
	require.Equal(t, digest([]byte(content)), receipt.SourceSHA256)
	require.Equal(t, digest(result.PDF()), receipt.PDFSHA256)
	require.NotEqual(t, receipt.SourceSHA256, receipt.PDFSHA256)
	require.Equal(t, policy.Fingerprint(), receipt.PolicyFingerprint)
	require.Equal(t, int64(len(content)), receipt.SourceBytes)
	require.Equal(t, int64(len(result.PDF())), receipt.PDFBytes)
	require.Equal(t, []Span{{1, 1, 1}, {1, 1, 2}, {1, 1, 3}, {1, 2, 1}, {1, 2, 2}, {1, 2, 3}, {1, 3, 1}}, receipt.Spans)
	pdfCopy := result.PDF()
	pdfCopy[0] = 0
	receipt.Spans[0].Page = 99
	receipt.PDFSHA256 = "changed"
	require.Equal(t, again.PDF(), result.PDF())
	require.Equal(t, again.Receipt(), result.Receipt())
	first, err := result.Source()
	require.NoError(t, err)
	second, err := result.Source()
	require.NoError(t, err)
	defer func() { require.NoError(t, first.Content.Close()); require.NoError(t, second.Content.Close()) }()
	_, err = first.Content.Read(make([]byte, 10))
	require.NoError(t, err)
	all, err := io.ReadAll(second.Content)
	require.NoError(t, err)
	require.Equal(t, result.PDF(), all)
	require.Equal(t, "application/pdf", second.MediaType)
	third, err := result.Source()
	require.NoError(t, err)
	_, err = io.Copy(mutatingWriter{}, third.Content)
	require.NoError(t, err)
	require.NoError(t, third.Content.Close())
	require.Equal(t, again.PDF(), result.PDF())
	require.Equal(t, again.Receipt(), result.Receipt())
}

type mutatingWriter struct{}

func (mutatingWriter) Write(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestPolicyBoundsAndIdentity(t *testing.T) {
	setters := []func(*Limits, int64){
		func(l *Limits, v int64) { l.MaxSourceBytes = v }, func(l *Limits, v int64) { l.MaxPDFBytes = v },
		func(l *Limits, v int64) { l.MaxRecords = int(v) }, func(l *Limits, v int64) { l.MaxCells = int(v) },
		func(l *Limits, v int64) { l.MaxCellBytes = int(v) }, func(l *Limits, v int64) { l.MaxPages = int(v) },
	}
	defaults := DefaultLimits()
	maxima := []int64{defaults.MaxSourceBytes, defaults.MaxPDFBytes, int64(defaults.MaxRecords), int64(defaults.MaxCells), int64(defaults.MaxCellBytes), int64(defaults.MaxPages)}
	base := policyFor(t, defaults)
	for index, set := range setters {
		for _, value := range []int64{-1, 0, maxima[index] + 1} {
			t.Run(fmt.Sprintf("limit_%d/value_%d", index, value), func(t *testing.T) {
				limits := defaults
				set(&limits, value)
				_, err := NewPolicy(limits)
				require.Error(t, err)
			})
		}
		limits := defaults
		set(&limits, maxima[index]-1)
		changed := policyFor(t, limits)
		require.NotEqual(t, base.Fingerprint(), changed.Fingerprint())
		require.Equal(t, changed.Fingerprint(), policyFor(t, limits).Fingerprint())
	}
	require.Empty(t, (Policy{}).Fingerprint())
}

func TestConcurrentConversionsRemainDeterministic(t *testing.T) {
	policy := policyFor(t, DefaultLimits())
	content := "café,κόσμος,Привет\n" + strings.Repeat("x,y,z\n", 40)
	source, _ := sourceFor(t, content)
	want, err := Convert(t.Context(), source, policy)
	require.NoError(t, err)
	for index := range 8 {
		t.Run(fmt.Sprintf("conversion_%d", index), func(t *testing.T) {
			t.Parallel()
			source, _ := sourceFor(t, content)
			got, err := Convert(t.Context(), source, policy)
			require.NoError(t, err)
			require.Equal(t, want.PDF(), got.PDF())
			require.Equal(t, want.Receipt(), got.Receipt())
		})
	}
}

func TestConvertRejectsInvalidSourceAndText(t *testing.T) {
	for _, content := range []string{"\n", "\"unclosed", "x\"y", "\xff", "你好", "مرحبا", "a\u0301", "x\tY", "x\rY", "x\u202eY", "x\u0000Y", "😀", "\U00010000"} {
		t.Run(digest([]byte(content))[:8], func(t *testing.T) {
			source, reader := sourceFor(t, content)
			result, err := Convert(t.Context(), source, policyFor(t, DefaultLimits()))
			require.Error(t, err)
			require.Nil(t, result)
			require.True(t, reader.closed)
		})
	}
	for _, change := range []func(*ocr.Source){
		func(s *ocr.Source) { s.Size++ }, func(s *ocr.Source) { s.Size-- }, func(s *ocr.Source) { s.SHA256 = strings.Repeat("0", 64) },
		func(s *ocr.Source) { s.MediaType = "application/pdf" }, func(s *ocr.Source) { s.MediaType = "Text/CSV" }, func(s *ocr.Source) { s.Size = 0 },
	} {
		source, reader := sourceFor(t, "x,y\n")
		change(&source)
		result, err := Convert(t.Context(), source, policyFor(t, DefaultLimits()))
		require.Error(t, err)
		require.Nil(t, result)
		require.True(t, reader.closed)
	}
}

func TestConvertBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, content string
		tighten       func(*Limits)
	}{
		{"source", "ab\n", func(l *Limits) { l.MaxSourceBytes = 3 }},
		{"records", "a\nb\n", func(l *Limits) { l.MaxRecords = 2 }},
		{"cells", "a,b\n", func(l *Limits) { l.MaxCells = 2 }},
		{"cell bytes", "éx\n", func(l *Limits) { l.MaxCellBytes = 3 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := DefaultLimits()
			test.tighten(&limits)
			source, _ := sourceFor(t, test.content)
			_, err := Convert(t.Context(), source, policyFor(t, limits))
			require.NoError(t, err)
			switch test.name {
			case "source":
				limits.MaxSourceBytes--
			case "records":
				limits.MaxRecords--
			case "cells":
				limits.MaxCells--
			case "cell bytes":
				limits.MaxCellBytes--
			}
			source, reader := sourceFor(t, test.content)
			result, err := Convert(t.Context(), source, policyFor(t, limits))
			require.Error(t, err)
			require.Nil(t, result)
			require.True(t, reader.closed)
		})
	}
	limits := DefaultLimits()
	limits.MaxPages = 1
	content := "\"" + strings.Repeat("x\n", 46) + "x\"\n"
	source, _ := sourceFor(t, content)
	one, err := Convert(t.Context(), source, policyFor(t, limits))
	require.NoError(t, err)
	require.Equal(t, 1, one.Receipt().Pages)
	source, _ = sourceFor(t, "\""+strings.Repeat("x\n", 47)+"x\"\n")
	result, err := Convert(t.Context(), source, policyFor(t, limits))
	require.Error(t, err)
	require.Nil(t, result)
	limits = DefaultLimits()
	limits.MaxPDFBytes = int64(len(one.PDF()))
	source, _ = sourceFor(t, content)
	_, err = Convert(t.Context(), source, policyFor(t, limits))
	require.NoError(t, err)
	limits.MaxPDFBytes--
	source, _ = sourceFor(t, content)
	result, err = Convert(t.Context(), source, policyFor(t, limits))
	require.Error(t, err)
	require.Nil(t, result)
}

func TestConvertLongCellMapping(t *testing.T) {
	source, _ := sourceFor(t, strings.Repeat("x", columns*linesPerPage)+"\n")
	result, err := Convert(t.Context(), source, policyFor(t, DefaultLimits()))
	require.NoError(t, err)
	require.Equal(t, []Span{{1, 1, 1}, {2, 1, 1}}, result.Receipt().Spans)
	pages, err := media.CountPDFPages(result.PDF())
	require.NoError(t, err)
	require.Equal(t, int64(2), pages)
	lines, err := layout(t.Context(), [][]string{{"first\n\nlast\n", ""}}, DefaultLimits())
	require.NoError(t, err)
	var texts []string
	for _, line := range lines {
		texts = append(texts, line.text)
	}
	require.Equal(t, []string{"Record 1, cell 1", "first", "", "last", "", "Record 1, cell 2", ""}, texts)
}

func TestConvertCancellationClosureAndZeroValues(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	source, reader := sourceFor(t, "x\n")
	result, err := Convert(ctx, source, policyFor(t, DefaultLimits()))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	require.True(t, reader.closed)
	source, reader = sourceFor(t, "x\n")
	result, err = Convert(t.Context(), source, Policy{})
	require.Error(t, err)
	require.Nil(t, result)
	require.True(t, reader.closed)
	source, reader = sourceFor(t, "x\n")
	reader.closeErr = errors.New("synthetic close failure")
	result, err = Convert(t.Context(), source, policyFor(t, DefaultLimits()))
	require.ErrorIs(t, err, reader.closeErr)
	require.Nil(t, result)
	for _, zero := range []*Result{nil, {}} {
		require.Empty(t, zero.PDF())
		require.Empty(t, zero.Receipt())
		_, err := zero.Source()
		require.Error(t, err)
	}
	_, err = Convert(t.Context(), ocr.Source{}, policyFor(t, DefaultLimits()))
	require.Error(t, err)
	ctx, cancel = context.WithCancel(t.Context())
	reader = &trackedReader{Reader: cancelReader{cancel: cancel}}
	source = ocr.Source{Content: reader, MediaType: "text/csv", Size: 1, SHA256: digest([]byte("x"))}
	result, err = Convert(ctx, source, policyFor(t, DefaultLimits()))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	require.True(t, reader.closed)
}

type cancelReader struct{ cancel context.CancelFunc }

func (r cancelReader) Read(p []byte) (int, error) {
	r.cancel()
	return copy(p, "x"), nil
}
