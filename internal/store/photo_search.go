package store

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
	return `EXISTS(SELECT 1 FROM photo_files f JOIN photo_assets a ON a.display_file_id=f.file_id JOIN nodes n ON n.id=f.node_id JOIN visual_preview_generations image_preview ON image_preview.content_version_id=` + version + ` WHERE f.asset_id=a.asset_id AND n.current_version_id=` + version + ` AND ` + photoDescriptionEligibility + ` AND image_preview.checksum=` + evidence + ` AND image_preview.state='ready')`
}
