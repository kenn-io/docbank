package api

import "go.kenn.io/docbank/internal/store"

type ProductionNumberReference struct {
	Label                   string `json:"label"`
	JobID                   string `json:"job_id"`
	SetID                   string `json:"set_id"`
	Revision                int64  `json:"revision"`
	ProductionReceiptSHA256 string `json:"production_receipt_sha256"`
	ArtifactManifestSHA256  string `json:"artifact_manifest_sha256"`
	SourceVersionID         string `json:"source_version_id"`
	OccurrenceID            string `json:"occurrence_id"`
	Page                    int    `json:"page"`
	ArtifactID              string `json:"artifact_id"`
	ArtifactSHA256          string `json:"artifact_sha256"`
	ArtifactPath            string `json:"artifact_path"`
	Volume                  string `json:"volume"`
}

type ProductionNumberPage struct {
	Items        []ProductionNumberReference `json:"items"`
	NextSequence int64                       `json:"next_sequence,omitzero"`
}

func productionNumberDTO(value store.PublishedProductionNumber) ProductionNumberReference {
	return ProductionNumberReference{Label: value.Label, JobID: value.JobID, SetID: value.SetID,
		Revision: value.Revision, ProductionReceiptSHA256: value.ProductionReceiptSHA256,
		ArtifactManifestSHA256: value.ArtifactManifestSHA256, SourceVersionID: value.SourceVersionID,
		OccurrenceID: value.OccurrenceID, Page: value.Page, ArtifactID: value.ArtifactID,
		ArtifactSHA256: value.ArtifactSHA256, ArtifactPath: value.ArtifactPath, Volume: value.Volume}
}
