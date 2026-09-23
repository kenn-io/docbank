package store

import (
	"context"
	"errors"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

func resolveExportEmailPDF(ctx context.Context, q metadataQuerier, version, generation string, policy bundle.RolePolicy, base string) (bundle.Role, error) {
	var receipt EmailPDFReceipt
	var err error
	if policy.ProfileFingerprint != "" {
		receipt, err = emailPDFReceipt(ctx, q, version, policy.ProfileFingerprint)
	} else {
		receipt, err = exportEmailPDFForRecipe(ctx, q, version, generation, policy.RecipeSHA256)
	}
	if err != nil {
		return bundle.Role{}, err
	}
	if generation != "" && receipt.Binding.GenerationID != generation {
		return bundle.Role{}, ErrNotFound
	}
	raw, err := canonical.Marshal(receipt)
	if err != nil {
		return bundle.Role{}, err
	}
	return bundle.Role{Role: "email_pdf", Status: roleAvailable, Path: base + "email.pdf", SHA256: receipt.Output.PDFSHA256, Size: receipt.Output.PDFSize, MediaType: "application/pdf", Recipe: raw}, nil
}

// Recipes are common across source-specific profiles. Select only a retained
// receipt for the requested recipe and attachment generation, when selected.
// Multiple remaining generations require an explicit profile.
func exportEmailPDFForRecipe(ctx context.Context, q metadataQuerier, version, generation, recipeSHA string) (EmailPDFReceipt, error) {
	var selected *EmailPDFReceipt
	after := ""
	for {
		profiles, err := exportEmailPDFProfiles(ctx, q, version, after)
		if err != nil {
			return EmailPDFReceipt{}, err
		}
		for _, profile := range profiles {
			receipt, err := emailPDFReceipt(ctx, q, version, profile)
			if err != nil {
				return EmailPDFReceipt{}, err
			}
			if generation != "" && receipt.Binding.GenerationID != generation {
				continue
			}
			raw, err := canonical.Marshal(receipt.Binding.Recipe)
			if err != nil {
				return EmailPDFReceipt{}, err
			}
			if pageChecksum(raw) != recipeSHA {
				continue
			}
			if selected != nil {
				return EmailPDFReceipt{}, bundle.ErrConflict
			}
			selected = &receipt
		}
		if len(profiles) < 100 {
			break
		}
		after = profiles[len(profiles)-1]
	}
	if selected == nil {
		return EmailPDFReceipt{}, ErrNotFound
	}
	return *selected, nil
}

func exportEmailPDFProfiles(ctx context.Context, q metadataQuerier, version, after string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT a.profile_fingerprint FROM rendition_attachments a JOIN rendition_artifacts r ON r.build_id=a.build_id WHERE a.content_version_id=? AND r.role=? AND a.profile_fingerprint>? ORDER BY a.profile_fingerprint LIMIT 100`, version, string(document.EvidenceArtifactPDF), after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	profiles := make([]string, 0, 100)
	for rows.Next() {
		var profile string
		if err = rows.Scan(&profile); err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	return profiles, errors.Join(rows.Err(), rows.Close())
}
