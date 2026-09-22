package production

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json/v2"
	"strconv"
	"strings"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
)

const (
	PrivilegeLogJSONMediaType = "application/json"
	PrivilegeLogCSVMediaType  = "text/csv; charset=utf-8"
)

// PrivilegeLogExport contains public bytes and the frozen authority needed to
// verify them. Content includes only PrivilegePublicRow fields.
type PrivilegeLogExport struct {
	MediaType     string
	ReceiptSHA256 string
	RowsSHA256    string
	ContentSHA256 string
	Content       []byte
}

func ExportPrivilegeLogJSON(receipt documentproduction.PrivilegeLogReceipt, withheld documentproduction.WithheldSelection, rows []documentproduction.PrivilegeRow) (PrivilegeLogExport, error) {
	ordered, err := verifyFrozenPrivilegeRows(receipt, withheld, rows)
	if err != nil {
		return PrivilegeLogExport{}, err
	}
	content, err := json.Marshal(publicPrivilegeRowsInOrder(ordered))
	if err != nil {
		return PrivilegeLogExport{}, err
	}
	content = append(content, '\n')
	return newPrivilegeLogExport(PrivilegeLogJSONMediaType, receipt, content), nil
}

func ExportPrivilegeLogCSV(receipt documentproduction.PrivilegeLogReceipt, withheld documentproduction.WithheldSelection, rows []documentproduction.PrivilegeRow) (PrivilegeLogExport, error) {
	ordered, err := verifyFrozenPrivilegeRows(receipt, withheld, rows)
	if err != nil {
		return PrivilegeLogExport{}, err
	}
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	if err := writer.Write([]string{"id", "withheld_member_id", "family_order", "source_version_id", "basis", "public_description"}); err != nil {
		return PrivilegeLogExport{}, err
	}
	for _, row := range publicPrivilegeRowsInOrder(ordered) {
		if err := writer.Write([]string{row.ID, row.WithheldMemberID, strconv.FormatInt(row.FamilyOrder, 10), row.SourceVersionID, safePrivilegeCSVCell(row.Basis), safePrivilegeCSVCell(row.PublicDescription)}); err != nil {
			return PrivilegeLogExport{}, err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return PrivilegeLogExport{}, err
	}
	return newPrivilegeLogExport(PrivilegeLogCSVMediaType, receipt, buffer.Bytes()), nil
}

func safePrivilegeCSVCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if (trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0]))) || strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") {
		return "'" + value
	}
	return value
}

type PreparedPrivilegeLogAttachment struct {
	OperationID   string
	RequestSHA256 string
	Canonical     []byte
	Receipt       documentproduction.PrivilegeLogAttachmentReceipt
}

// PreparePrivilegeLogAttachment records later produced-number/artifact
// references without changing the frozen privilege-log receipt. Final
// production integration remains responsible for matching the supplied shared
// production and artifact digests to retained artifact authority.
func PreparePrivilegeLogAttachment(operationID, attachmentID string, receipt documentproduction.PrivilegeLogReceipt, withheld documentproduction.WithheldSelection, rows []documentproduction.PrivilegeRow, productionReceiptSHA256 string, references []documentproduction.PrivilegeOutputReference, createdAt time.Time) (PreparedPrivilegeLogAttachment, error) {
	if !canonicalUUIDv4(operationID) || !canonicalUUIDv4(attachmentID) || !canonicalSHA256(productionReceiptSHA256) || !canonicalUTCTime(createdAt) {
		return PreparedPrivilegeLogAttachment{}, invalidProductionContract("invalid privilege log attachment request")
	}
	if _, err := verifyFrozenPrivilegeRows(receipt, withheld, rows); err != nil {
		return PreparedPrivilegeLogAttachment{}, err
	}
	memberIDs := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		memberIDs[row.WithheldMemberID] = struct{}{}
	}
	for _, reference := range references {
		if _, exists := memberIDs[reference.WithheldMemberID]; !exists {
			return PreparedPrivilegeLogAttachment{}, invalidProductionContract("privilege output reference is outside frozen rows")
		}
	}
	value := documentproduction.PrivilegeLogAttachmentReceipt{
		Contract: documentproduction.PrivilegeLogAttachmentContractV1, ID: attachmentID,
		PrivilegeLogReceiptSHA256: receipt.SHA256, ProductionReceiptSHA256: productionReceiptSHA256,
		References: references, CreatedAt: createdAt.Format(time.RFC3339Nano),
	}
	canonical, digest, err := documentproduction.CanonicalPrivilegeLogAttachment(value)
	if err != nil {
		return PreparedPrivilegeLogAttachment{}, err
	}
	value.SHA256 = digest
	return PreparedPrivilegeLogAttachment{
		OperationID: operationID, RequestSHA256: digest, Canonical: canonical, Receipt: value,
	}, nil
}

func verifyFrozenPrivilegeRows(receipt documentproduction.PrivilegeLogReceipt, withheld documentproduction.WithheldSelection, rows []documentproduction.PrivilegeRow) ([]documentproduction.PrivilegeRow, error) {
	if err := documentproduction.ValidatePrivilegeLogReceipt(receipt); err != nil {
		return nil, err
	}
	if err := documentproduction.ValidateWithheldSelection(withheld); err != nil {
		return nil, err
	}
	_, digest, err := documentproduction.CanonicalPrivilegeRows(rows)
	if err != nil {
		return nil, err
	}
	if digest != receipt.RowsSHA256 || len(rows) != receipt.RowCount || withheld.SHA256 != receipt.WithheldSelectionSHA256 {
		return nil, &documentproduction.Problem{
			Code: documentproduction.ProblemPrivilegeLogStale, Detail: "export rows differ from frozen privilege log", SubjectID: receipt.LogID,
		}
	}
	ordered, err := documentproduction.OrderPrivilegeRows(withheld, rows)
	if err != nil {
		return nil, err
	}
	return ordered, nil
}

func publicPrivilegeRowsInOrder(rows []documentproduction.PrivilegeRow) []documentproduction.PrivilegePublicRow {
	public := make([]documentproduction.PrivilegePublicRow, 0, len(rows))
	for _, row := range rows {
		public = append(public, documentproduction.PublicPrivilegeRows([]documentproduction.PrivilegeRow{row})[0])
	}
	return public
}

func newPrivilegeLogExport(mediaType string, receipt documentproduction.PrivilegeLogReceipt, content []byte) PrivilegeLogExport {
	digest := sha256.Sum256(content)
	return PrivilegeLogExport{
		MediaType: mediaType, ReceiptSHA256: receipt.SHA256, RowsSHA256: receipt.RowsSHA256,
		ContentSHA256: hex.EncodeToString(digest[:]), Content: content,
	}
}
