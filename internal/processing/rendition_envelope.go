package processing

import (
	"mime"
	"path/filepath"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func envelopeRenditionJobV1(
	work store.RenditionJobWork, profile document.ProcessingProfileV1,
	kind document.EvidenceUnitKind, rendition document.RenditionV1,
) (document.RenditionV1, error) {
	metadata := work.ExecutionIdentity.Upload
	enveloped, _, err := document.EnvelopeRenditionV1(rendition, document.RenditionEnvelopeV1{
		BuildID: work.Job.ID, SourceSHA256: work.Job.SourceSHA256,
		SourceFormat:                sourceFormat(metadata.Filename, metadata.MediaFamily, metadata.MediaType),
		SourceMediaType:             metadata.MediaType,
		RenditionRequestFingerprint: work.Job.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:  work.Job.EvidenceLexicalFingerprint,
		NormalizedEvidenceContract:  profile.EvidenceLexical.NormalizedEvidenceContract,
		UnitKind:                    kind,
	})
	return enveloped, err
}

func sourceFormat(filename, family, mediaType string) string {
	basename := filepath.Base(filename)
	extension := filepath.Ext(basename)
	if extension == basename {
		extension = ""
	} else {
		extension = strings.TrimPrefix(strings.ToLower(extension), ".")
	}
	if extension != "" {
		return extension
	}
	base, _, _ := mime.ParseMediaType(mediaType)
	if _, subtype, found := strings.Cut(base, "/"); found && subtype != "" {
		return subtype
	}
	return family
}
