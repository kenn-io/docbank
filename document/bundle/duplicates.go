package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

func ValidateDuplicatePolicy(policy string) error {
	if policy != "" && policy != "preserve" && policy != "collapse_exact_content" {
		return ErrConflict
	}
	return nil
}

// DuplicateCursor preserves every source and attachment receipt. Explicit
// collapse shares only an identical output at the same family position, from
// equal original EML bytes and equal rendering recipes. Subjects and Message-ID
// never enter this decision. State is bounded by the existing MaxRoles limit.
type DuplicateCursor struct {
	Policy string
	seen   map[string]string
}

func (c *DuplicateCursor) Add(d *Document, seal bool) error {
	if err := ValidateDuplicatePolicy(c.Policy); err != nil {
		return err
	}
	for i := range d.Roles {
		r := &d.Roles[i]
		if c.Policy != "collapse_exact_content" || r.Status == "unavailable" {
			if r.Status == "collapsed" || r.ReuseOf != "" {
				return ErrConflict
			}
			continue
		}
		if r.Status != "available" && r.Status != "collapsed" {
			return ErrConflict
		}
		key, err := collapseKey(*d, *r)
		if err != nil {
			return err
		}
		if c.seen == nil {
			c.seen = map[string]string{}
		}
		previous := c.seen[key]
		if seal && previous != "" {
			r.Status, r.ReuseOf = "collapsed", previous
		}
		if r.Status == "collapsed" {
			if previous == "" || previous != r.ReuseOf || r.Path == previous {
				return ErrConflict
			}
		} else {
			if previous != "" || r.ReuseOf != "" {
				return ErrConflict
			}
			if len(c.seen) >= MaxRoles {
				return ErrLimit
			}
			c.seen[key] = r.Path
		}
	}
	return nil
}

func collapseKey(d Document, r Role) (string, error) {
	part := ""
	if d.Attachment != nil {
		part = d.Attachment.PartPath
	}
	recipe := r.Recipe
	if r.Role == "email_pdf" || r.Role == "attachment_pdf" {
		var receipt document.EmailPDFReceiptV1
		if json.Unmarshal(r.Recipe, &receipt, json.RejectUnknownMembers(true)) != nil {
			return "", ErrConflict
		}
		var err error
		recipe, err = canonical.Marshal(receipt.Binding)
		if err != nil {
			return "", err
		}
	}
	page := 0
	if r.Page != nil {
		page = r.Page.Page
	}
	raw, err := canonical.Marshal(struct {
		Source      string         `json:"source"`
		SourceBytes int64          `json:"source_bytes"`
		Part        string         `json:"part"`
		Role        string         `json:"role"`
		SHA256      string         `json:"sha256"`
		Size        int64          `json:"size"`
		MediaType   string         `json:"media_type"`
		Recipe      jsontext.Value `json:"recipe,omitzero"`
		Page        int            `json:"page"`
	}{d.SHA256, d.Size, part, r.Role, r.SHA256, r.Size, r.MediaType, recipe, page})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}
