package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/report"
)

func validTermReportID(id string) bool {
	if len(id) != 48 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == lowerHex(id)
}

func lowerHex(value string) string {
	bytes, _ := hex.DecodeString(value)
	return hex.EncodeToString(bytes)
}

func validateTermReportSummary(summary report.Summary) error {
	if !validTermReportID(summary.ID) || summary.ObservedAt.IsZero() ||
		!summary.ExpiresAt.After(summary.ObservedAt) || len(summary.Terms) == 0 ||
		len(summary.Terms) > 128 {
		return errors.New("report summary lacks bounded frozen authority")
	}
	switch summary.State {
	case "complete":
		if len(summary.Counts) != len(summary.Terms) || summary.CSVBytes < 1 ||
			summary.BundleBytes < 1 || len(summary.CSVSHA256) != 64 || len(summary.BundleSHA256) != 64 {
			return errors.New("complete report summary lacks artifact authority")
		}
	case "needs_review":
		if len(summary.Counts) != 0 || summary.CSVBytes != 0 || summary.BundleBytes != 0 {
			return errors.New("review report summary exposes incomplete counts")
		}
	default:
		return errors.New("report summary has an unknown state")
	}
	return nil
}

func (c *Connection) CreateTermReport(ctx context.Context, request report.Request) (report.Summary, error) {
	request, err := report.NormalizeRequest(request)
	if err != nil {
		return report.Summary{}, err
	}
	response, err := c.API().CreateTermReport(ctx, &apiclient.CreateTermReportRequestOptions{Body: &request})
	if err != nil {
		return report.Summary{}, err
	}
	if err := validateTermReportSummary(*response); err != nil {
		return report.Summary{}, err
	}
	return *response, nil
}

func (c *Connection) GetTermReport(ctx context.Context, id string) (report.Summary, error) {
	if !validTermReportID(id) {
		return report.Summary{}, errors.New("report ID must be 48 lowercase hexadecimal characters")
	}
	response, err := c.API().GetTermReport(ctx, &apiclient.GetTermReportRequestOptions{
		PathParams: &apiclient.GetTermReportPath{ID: id}})
	if err != nil {
		return report.Summary{}, err
	}
	if err := validateTermReportSummary(*response); err != nil || response.ID != id {
		return report.Summary{}, errors.New("report summary differs from requested handle")
	}
	return *response, nil
}

func (c *Connection) TermReportDates(ctx context.Context, id string, page report.DatePageRequest) (report.DatePage, error) {
	if !validTermReportID(id) {
		return report.DatePage{}, errors.New("report ID must be 48 lowercase hexadecimal characters")
	}
	if page.Limit < 0 || page.Limit > 100 || len(page.Cursor) > 4096 {
		return report.DatePage{}, errors.New("invalid report date page bounds")
	}
	response, err := c.API().GetTermReportDates(ctx, &apiclient.GetTermReportDatesRequestOptions{
		PathParams: &apiclient.GetTermReportDatesPath{ID: id}, Body: &page})
	if err != nil {
		return report.DatePage{}, err
	}
	if len(response.Members) > 100 || len(response.NextCursor) > 4096 {
		return report.DatePage{}, errors.New("report date page exceeds response limits")
	}
	return *response, nil
}

func (c *Connection) ReviseTermReport(ctx context.Context, id string, choices []report.DateChoice) (report.Summary, error) {
	if !validTermReportID(id) {
		return report.Summary{}, errors.New("report ID must be 48 lowercase hexadecimal characters")
	}
	response, err := c.API().ReviseTermReport(ctx, &apiclient.ReviseTermReportRequestOptions{
		PathParams: &apiclient.ReviseTermReportPath{ID: id},
		Body:       &apiclient.ReviseTermReportBody{Choices: choices}})
	if err != nil {
		return report.Summary{}, err
	}
	if err := validateTermReportSummary(*response); err != nil || response.ParentID != id {
		return report.Summary{}, errors.New("report revision differs from requested parent")
	}
	return *response, nil
}

// TermReportStream validates the size and digest advertised by the daemon
// before the caller publishes a privately staged output file.
type TermReportStream struct {
	io.ReadCloser

	Size   int64
	SHA256 string
}

func (s *TermReportStream) CopyVerified(output io.Writer) (int64, error) {
	if s == nil || s.ReadCloser == nil || output == nil || s.Size < 0 || s.Size > 512<<20 || len(s.SHA256) != 64 {
		return 0, errors.New("invalid report download authority")
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(s, s.Size+1))
	if err != nil {
		return written, err
	}
	if written != s.Size || hex.EncodeToString(hash.Sum(nil)) != s.SHA256 {
		return written, errors.New("report download differs from advertised size or digest")
	}
	return written, nil
}

func (c *Connection) OpenTermReport(ctx context.Context, id, format string) (*TermReportStream, error) {
	if !validTermReportID(id) || (format != "csv" && format != "bundle") {
		return nil, errors.New("invalid report handle or format")
	}
	var response *http.Response
	var err error
	if format == "csv" {
		_, err = c.apiWithResponse(&response).DownloadTermReportcsv(runtime.WithStreamingResponse(ctx),
			&apiclient.DownloadTermReportcsvRequestOptions{PathParams: &apiclient.DownloadTermReportcsvPath{ID: id}})
	} else {
		_, err = c.apiWithResponse(&response).DownloadTermReportbundle(runtime.WithStreamingResponse(ctx),
			&apiclient.DownloadTermReportbundleRequestOptions{PathParams: &apiclient.DownloadTermReportbundlePath{ID: id}})
	}
	if err != nil {
		return nil, err
	}
	if response == nil || response.Body == nil {
		return nil, errors.New("report download has no response body")
	}
	size, err := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64)
	digest := response.Header.Get("X-Docbank-Report-Sha256")
	if err != nil || size < 0 || size > 512<<20 || len(digest) != 64 {
		_ = response.Body.Close()
		return nil, errors.New("report download lacks bounded size or digest")
	}
	return &TermReportStream{ReadCloser: response.Body, Size: size, SHA256: digest}, nil
}
