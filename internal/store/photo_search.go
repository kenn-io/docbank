package store

import "go.kenn.io/docbank/document"

// PhotoRanking pins ranked asset membership and retrieval mode for a snapshot.
type PhotoRanking struct {
	CoverageIncomplete bool             `json:"coverage_incomplete,omitempty"`
	Truncated          bool             `json:"truncated,omitempty"`
	RequestedMode      string           `json:"requested_mode"`
	ActualMode         string           `json:"actual_mode"`
	FallbackReason     string           `json:"fallback_reason,omitempty"`
	Members            []SnapshotMember `json:"-"`
}

// currentImageEmbeddingSQL accepts catalog-owned SQL expressions.
func currentImageEmbeddingSQL(version, evidence string) string {
	return currentImageEmbeddingPreviewSQL(version, `image_preview.checksum=`+evidence)
}

func currentImageEmbeddingRecipeSQL(version, evidence, recipe string) string {
	return currentImageEmbeddingPreviewSQL(version, `image_preview.checksum=`+evidence+` AND image_preview.recipe_fingerprint=`+recipe)
}

func currentImageEmbeddingPreviewSQL(version, previewPredicate string) string {
	return `EXISTS(SELECT 1 FROM content_versions image_version JOIN nodes n ON n.id=image_version.node_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.display_file_id=f.file_id JOIN visual_preview_generations image_preview ON image_preview.content_version_id=image_version.version_id WHERE image_version.version_id=` + version + ` AND f.asset_id=a.asset_id AND n.current_version_id=image_version.version_id AND ` + photoDescriptionEligibility + ` AND ` + previewPredicate + ` AND image_preview.state='ready' AND image_preview.contract_version='` + document.VisualPreviewContractV1 + `')`
}
