package store

import (
	"fmt"
	"strings"
)

type backendMaintenance struct {
	name     string
	owner    string
	boundary string
	triggers string
}

// Descriptive ownership inventory, grouped by effect. The named contract
// suites exercise the ported effects through native operations.
var backendMaintenanceEffects = []backendMaintenance{
	{"checksums", "content_checksums.go; INSERT-only runtime grant", "checksums contracts; no UPDATE path", `
 blob_checksums_immutable_update`},
	{"provenance", "ingest.go, provenance.go and provenance_bindings.go", "imports/sources contracts; immutable inserts and same-node predecessor validation", `
 ingests_immutable_update provenance_immutable_update provenance_same_node_insert
 provenance_version_bindings_immutable_update`},
	{"pack summaries", "pack_maintenance.go: refreshPackSummariesTx", "documents/physical-storage contracts", `
 blob_pack_summary_mapping_insert blob_pack_summary_mapping_delete
 blob_pack_summary_mapping_update blob_pack_summary_blob_delete blob_pack_summary_blob_insert`},
	{"content promotion", "document_maintenance.go: promoteContentBlobTx", "documents contracts", `
 content_versions_promote_rendition_staging content_versions_revoke_derivative_purge`},
	{"invalidation epochs", "document_events.go and retention_maintenance.go", "sources/retention/metadata-events contracts; people publication remains unqualified", `
 document_event_heads_deleted content_versions_invalidate_person_bindings`},
	{"photo timeline", "document_maintenance.go: refreshPhotoTimelineTx", "documents/imports/photo-import/photo-checkout/sources/retention contracts; SQLite pending table is a trigger intermediate", `
 photo_timeline_refresh photo_timeline_file_insert photo_timeline_role_change
 photo_timeline_version_delete photo_timeline_version_change
 photo_timeline_binding_insert photo_timeline_correction_insert`},
	{"photo export queue", "document_maintenance.go: refreshPhotoAssetWorkTx and refreshNodePhotoWorkTx", "documents/photo-import/photo-checkout/trash/retention/organization/photo-edit contracts", `
