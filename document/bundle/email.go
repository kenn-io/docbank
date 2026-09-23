package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

// ValidateEmailPDFRoles checks portable receipt consistency, not authenticity
// or renderer execution. The store establishes retained authority when sealing;
// archive consumers can independently reject contradictory source/recipe/output
// declarations even when an archive has internally consistent checksums.
func ValidateEmailPDFRoles(plan Plan, d Document) error {
	var policy *RolePolicy
	for i := range plan.Roles {
		if plan.Roles[i].Role == "email_pdf" {
			if policy != nil {
				return ErrConflict
			}
			policy = &plan.Roles[i]
		}
	}
	count := 0
	for _, role := range d.Roles {
		if role.Role != "email_pdf" {
			continue
		}
		count++
		if role.Status == "collapsed" {
			role.Status = "available"
		}
		if policy == nil || count != 1 || (policy.ProfileFingerprint == "") == (policy.RecipeSHA256 == "") {
			return ErrConflict
		}
		if role.Status == "unavailable" {
			if !policy.AllowUnavailable || role.Path != "" || role.SHA256 != "" || role.Size != 0 || len(role.Recipe) != 0 || role.Page != nil {
				return ErrConflict
			}
			continue
		}
		var receipt document.EmailPDFReceiptV1
		if role.Status != "available" || json.Unmarshal(role.Recipe, &receipt, json.RejectUnknownMembers(true)) != nil {
			return ErrConflict
		}
		if receipt.Source != (document.EmailDocumentIdentity{NodeID: d.NodeID, VersionID: d.VersionID, SHA256: d.SHA256, Size: d.Size}) || document.ValidateEmailDocumentIdentity(receipt.Source) != nil {
			return ErrConflict
		}
		profile, err := document.EmailPDFProfile(receipt.Binding)
		if err != nil {
			return ErrConflict
		}
		_, fingerprints, err := document.CanonicalProfile(profile)
		if err != nil || fingerprints.Profile != receipt.ProfileFingerprint {
			return ErrConflict
		}
		recipe, err := canonical.Marshal(receipt.Binding.Recipe)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(recipe)
		if policy.ProfileFingerprint != "" && policy.ProfileFingerprint != receipt.ProfileFingerprint || policy.RecipeSHA256 != "" && policy.RecipeSHA256 != hex.EncodeToString(sum[:]) {
			return ErrConflict
		}
		o := receipt.Output
		if !canonical.IsSHA256Hex(receipt.AttachmentID) || !canonical.IsSHA256Hex(receipt.BuildID) || document.ValidateEmailPartPath(o.BodyPath) != nil || !canonical.IsSHA256Hex(o.BodySHA256) || o.BodySize < 1 || o.BodySize > 16<<20 || o.Pages < 1 || o.Pages > 1000 || o.PDFSize < 1 || o.PDFSize > 256<<20 || !canonical.IsSHA256Hex(o.PDFSHA256) {
			return ErrConflict
		}
		if role.Path != fmt.Sprintf("documents/%d/%s/email.pdf", d.NodeID, d.VersionID) || role.SHA256 != o.PDFSHA256 || role.Size != o.PDFSize || role.MediaType != "application/pdf" || role.Page != nil {
			return ErrConflict
		}
	}
	if policy != nil && count != 1 {
		return ErrConflict
	}
	return nil
}
