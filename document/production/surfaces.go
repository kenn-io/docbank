package production

import "slices"

const (
	methodGET  = "GET"
	methodPOST = "POST"
)

type SurfaceOperation struct {
	OperationID string `json:"operation_id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Tool        string `json:"tool"`
	Transport   string `json:"transport"`
}

var surfaceOperations = []SurfaceOperation{
	{"listProductionRecipes", methodGET, "/api/v1/production-sets/recipes", "production_recipes", GeneratedClientTransport},
	{"createProductionSet", methodPOST, "/api/v1/production-sets", "production_create", GeneratedClientTransport},
	{"listProductionSets", methodGET, "/api/v1/production-sets", "production_list", GeneratedClientTransport},
	{"readProductionSet", methodGET, "/api/v1/production-sets/{set}", "production_show", GeneratedClientTransport},
	{"forkProductionDraft", methodPOST, "/api/v1/production-sets/{set}/revisions", "production_fork", GeneratedClientTransport},
	{"readProductionRevision", methodGET, "/api/v1/production-sets/{set}/revisions/{revision}", "production_revision_show", GeneratedClientTransport},
	{"appendProductionMembers", methodPOST, "/api/v1/production-sets/{set}/revisions/{revision}/members", "production_members_add", GeneratedClientTransport},
	{"sealProductionMembers", methodPOST, "/api/v1/production-sets/{set}/revisions/{revision}/members/seal", "production_members_seal", GeneratedClientTransport},
	{"listProductionMembers", methodGET, "/api/v1/production-sets/{set}/revisions/{revision}/members", "production_members_list", GeneratedClientTransport},
	{"setProductionInstructions", methodPOST, "/api/v1/production-sets/{set}/revisions/{revision}/instructions", "production_instructions_set", GeneratedClientTransport},
	{"applyProductionChanges", methodPOST, "/api/v1/production-sets/{set}/revisions/{revision}/changes", "production_changes_apply", GeneratedClientTransport},
	{"resolveProductionSelection", methodPOST, "/api/v1/production-sets/{set}/revisions/{revision}/resolve", "production_resolve", GeneratedClientTransport},
	{"listProductionDecisions", methodGET, "/api/v1/production-sets/{set}/revisions/{revision}/decisions", "production_decisions_list", GeneratedClientTransport},
	{"readProductionMap", methodGET, "/api/v1/production-sets/{set}/revisions/{revision}/maps/{member}", "production_map", GeneratedClientTransport},
	{"declareProductionReview", methodPOST, "/api/v1/production-sets/{set}/revisions/{revision}/reviews", "production_review", GeneratedClientTransport},
	{"createProductionPreview", methodPOST, "/api/v1/production-sets/{set}/revisions/{revision}/previews", "production_preview", GeneratedClientTransport},
	{"finalizeProduction", methodPOST, "/api/v1/production-sets/{set}/revisions/{revision}/finalize", "production_finalize", GeneratedClientTransport},
	{"createProductionJob", methodPOST, "/api/v1/production-sets/{set}/revisions/{revision}/jobs", "production_run", GeneratedClientTransport},
	{"readProductionJob", methodGET, "/api/v1/production-sets/{set}/jobs/{job}", "production_status", GeneratedClientTransport},
	{"cancelProductionJob", methodPOST, "/api/v1/production-sets/{set}/jobs/{job}/cancel", "production_cancel", GeneratedClientTransport},
	{"createProductionDownload", methodPOST, "/api/v1/production-sets/{set}/jobs/{job}/download", "production_download", GeneratedClientTransport},
	{"createProductionPolicyVersion", methodPOST, "/api/v1/production-policies", "production_policy_create", GeneratedClientTransport},
	{"listProductionPolicyVersions", methodGET, "/api/v1/production-policies", "production_policy_list", GeneratedClientTransport},
	{"readProductionPolicyVersion", methodGET, "/api/v1/production-policies/{policy}/versions/{version}", "production_policy_show", GeneratedClientTransport},
	{"recordProductionApproval", methodPOST, "/api/v1/production-approvals", "production_approval_record", GeneratedClientTransport},
	{"readProductionApproval", methodGET, "/api/v1/production-approvals/{approval}", "production_approval_show", GeneratedClientTransport},
	{"revokeProductionApproval", methodPOST, "/api/v1/production-approvals/{approval}/revocations", "production_approval_revoke", GeneratedClientTransport},
	{"supersedeProductionApproval", methodPOST, "/api/v1/production-approvals/{approval}/supersessions", "production_approval_supersede", GeneratedClientTransport},
	{"createProductionPrivilegeLog", methodPOST, "/api/v1/production-privilege-logs", "production_privilege_log_create", GeneratedClientTransport},
	{"applyProductionPrivilegeRows", methodPOST, "/api/v1/production-privilege-logs/{log}/rows", "production_privilege_log_rows_apply", GeneratedClientTransport},
	{"readProductionPrivilegeLog", methodGET, "/api/v1/production-privilege-logs/{log}", "production_privilege_log_show", GeneratedClientTransport},
	{"validateProductionPrivilegeLog", methodPOST, "/api/v1/production-privilege-logs/{log}/validations", "production_privilege_log_validate", GeneratedClientTransport},
	{"freezeProductionPrivilegeLog", methodPOST, "/api/v1/production-privilege-logs/{log}/freeze", "production_privilege_log_freeze", GeneratedClientTransport},
	{"exportProductionPrivilegeLog", methodPOST, "/api/v1/production-privilege-logs/{log}/exports", "production_privilege_log_export", GeneratedClientTransport},
	{"readProductionRetention", methodGET, "/api/v1/production-receipts/{receipt}/retention", "production_retention_show", GeneratedClientTransport},
	{"createProductionReproduction", methodPOST, "/api/v1/production-receipts/{receipt}/reproductions", "production_reproduce", GeneratedClientTransport},
	{"readProductionReproduction", methodGET, "/api/v1/production-reproductions/{reproduction}", "production_reproduction_show", GeneratedClientTransport},
}

func SurfaceOperations() []SurfaceOperation { return slices.Clone(surfaceOperations) }

const (
	StorageWriterCoordinator  = "coordinator"
	StorageWriterPolicy       = "policy-service"
	StorageWriterPrivilege    = "privilege-log-service"
	StorageWriterJob          = "production-job-service"
	StorageWriterRetention    = "retention-service"
	StorageWriterReproduction = "reproduction-service"

	StorageImmutable  = "immutable"
	StorageAppendOnly = "append-only"
)

type StorageContract struct {
	RecordKind string `json:"record_kind"`
	Writer     string `json:"writer"`
	Mutability string `json:"mutability"`
	BlobRoot   bool   `json:"blob_root"`
}

var storageContracts = []StorageContract{
	{"production_schema_and_registry", StorageWriterCoordinator, StorageImmutable, false},
	{"production_policy_version", StorageWriterPolicy, StorageImmutable, false},
	{"production_approval_grant", StorageWriterPolicy, StorageImmutable, false},
	{"production_approval_event", StorageWriterPolicy, StorageAppendOnly, false},
	{"production_players_snapshot", StorageWriterPrivilege, StorageImmutable, false},
	{"production_withheld_selection", StorageWriterPrivilege, StorageImmutable, false},
	{"production_privilege_log_row", StorageWriterPrivilege, StorageAppendOnly, false},
	{"production_privilege_log_receipt", StorageWriterPrivilege, StorageImmutable, false},
	{"production_privilege_log_attachment", StorageWriterPrivilege, StorageImmutable, false},
	{"production_job_receipt", StorageWriterJob, StorageImmutable, true},
	{"production_artifact_manifest", StorageWriterJob, StorageImmutable, true},
	{"production_artifact_provenance", StorageWriterRetention, StorageImmutable, true},
	{"production_retention_receipt", StorageWriterRetention, StorageImmutable, true},
	{"production_reproduction_receipt", StorageWriterReproduction, StorageImmutable, true},
}

func StorageContracts() []StorageContract { return slices.Clone(storageContracts) }
