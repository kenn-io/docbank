package production

import "slices"

const (
	methodGET  = "GET"
	methodPOST = "POST"
	methodPUT  = "PUT"
)

type SurfaceOperation struct {
	OperationID string `json:"operation_id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Tool        string `json:"tool"`
	Transport   string `json:"transport"`
}

// surfaceOperations lists only the production routes registered in the daemon.
// A blank Tool means the operation has no dedicated MCP tool. Human approval
// issuance remains an authenticated embedded-host operation, not a daemon route.
var surfaceOperations = []SurfaceOperation{
	{"admitProductionJob", methodPOST, "/api/v1/productions/sets/{set_id}/revisions/{revision}/jobs", "admit_production_job", GeneratedClientTransport},
	{"appendProductionMembers", methodPOST, "/api/v1/productions/sets/{set_id}/revisions/{revision}/members", "append_production_members", GeneratedClientTransport},
	{"applyProductionChanges", methodPOST, "/api/v1/productions/sets/{set_id}/revisions/{revision}/changes", "apply_production_changes", GeneratedClientTransport},
	{"cancelProductionJob", methodPOST, "/api/v1/productions/sets/{set_id}/jobs/{job_id}/cancel", "cancel_production_job", GeneratedClientTransport},
	{"createProductionPackage", methodPOST, "/api/v1/productions/jobs/{job_id}/packages", "create_production_package", GeneratedClientTransport},
	{"createProductionPlayersSnapshot", methodPOST, "/api/v1/production-player-snapshots/{snapshot_id}/revisions/{revision}", "create_production_players_snapshot", GeneratedClientTransport},
	{"createProductionPolicyVersion", methodPOST, "/api/v1/productions/policies", "create_production_policy", GeneratedClientTransport},
	{"createProductionPreview", methodPOST, "/api/v1/productions/sets/{set_id}/revisions/{revision}/previews", "", GeneratedClientTransport},
	{"createProductionPrivilegeLogDraft", methodPOST, "/api/v1/production-privilege-logs/{log}/revisions/{revision}/draft", "create_production_privilege_draft", GeneratedClientTransport},
	{"createProductionReproduction", methodPOST, "/api/v1/productions/jobs/{job_id}/reproductions", "create_production_reproduction", GeneratedClientTransport},
	{"createProductionSet", methodPOST, "/api/v1/productions/sets", "create_production_set", GeneratedClientTransport},
	{"createProductionSupplement", methodPOST, "/api/v1/productions/supplements", "create_production_supplement", GeneratedClientTransport},
	{"createProductionWithheldSelection", methodPOST, "/api/v1/productions/sets/{set_id}/revisions/{revision}/withheld-selection", "create_production_withheld_selection", GeneratedClientTransport},
	{"downloadProductionPackage", methodPOST, "/api/v1/productions/jobs/{job_id}/packages/{operation_id}/download", "download_production_package", GeneratedClientTransport},
	{"editProductionInstructions", methodPUT, "/api/v1/productions/sets/{set_id}/revisions/{revision}/instructions", "edit_production_instructions", GeneratedClientTransport},
	{"exportProductionPrivilegeLog", methodGET, "/api/v1/production-privilege-logs/{log}/revisions/{revision}/exports/{format}", "export_production_privilege_log", GeneratedClientTransport},
	{"finalizeProductionDraft", methodPOST, "/api/v1/productions/sets/{set_id}/revisions/{revision}/finalize", "finalize_production_draft", GeneratedClientTransport},
	{"findProductionNumberCandidates", methodGET, "/api/v1/productions/numbers/candidates", "find_production_number_candidates", GeneratedClientTransport},
	{"findProductionNumbers", methodGET, "/api/v1/productions/numbers", "find_production_numbers", GeneratedClientTransport},
	{"forkProductionDraft", methodPOST, "/api/v1/productions/sets/{set_id}/revisions/{revision}/fork", "fork_production_draft", GeneratedClientTransport},
	{"freezeProductionPrivilegeLog", methodPOST, "/api/v1/production-privilege-logs/{log}/revisions/{revision}/freeze", "freeze_production_privilege_log", GeneratedClientTransport},
	{"getProductionDraft", methodGET, "/api/v1/productions/sets/{set_id}/revisions/{revision}", "get_production_draft", GeneratedClientTransport},
	{"getProductionJobStatus", methodGET, "/api/v1/productions/sets/{set_id}/jobs/{job_id}", "get_production_job", GeneratedClientTransport},
	{"getProductionMapChunk", methodGET, "/api/v1/productions/sets/{set_id}/revisions/{revision}/maps/{member_id}", "", GeneratedClientTransport},
	{"getProductionPackage", methodGET, "/api/v1/productions/jobs/{job_id}/packages/{operation_id}", "get_production_package", GeneratedClientTransport},
	{"getProductionReproduction", methodGET, "/api/v1/productions/jobs/{job_id}/reproductions/{operation_id}", "get_production_reproduction", GeneratedClientTransport},
	{"getProductionSet", methodGET, "/api/v1/productions/sets/{set_id}", "get_production_set", GeneratedClientTransport},
	{"getProductionSupplement", methodGET, "/api/v1/productions/supplements/{operation_id}", "get_production_supplement", GeneratedClientTransport},
	{"listProductionDecisions", methodGET, "/api/v1/productions/sets/{set_id}/revisions/{revision}/decisions", "list_production_decisions", GeneratedClientTransport},
	{"listProductionMembers", methodGET, "/api/v1/productions/sets/{set_id}/revisions/{revision}/members", "list_production_members", GeneratedClientTransport},
	{"listProductionPolicyVersions", methodGET, "/api/v1/productions/policies", "list_production_policies", GeneratedClientTransport},
	{"listProductionRecipes", methodGET, "/api/v1/productions/recipes", "list_production_recipes", GeneratedClientTransport},
	{"listProductionSets", methodGET, "/api/v1/productions/sets", "list_production_sets", GeneratedClientTransport},
	{"publishProductionPackage", methodPOST, "/api/v1/productions/jobs/{job_id}/packages/publish", "publish_production_package", GeneratedClientTransport},
	{"readProductionApproval", methodGET, "/api/v1/production-approvals/{approval}", "get_production_approval", GeneratedClientTransport},
	{"readProductionPolicyVersion", methodGET, "/api/v1/productions/policies/{policy_id}/versions/{version}", "get_production_policy", GeneratedClientTransport},
	{"readProductionPrivilegeLog", methodGET, "/api/v1/production-privilege-logs/{log}", "get_production_privilege_log", GeneratedClientTransport},
	{"replaceProductionPrivilegeLogRows", methodPOST, "/api/v1/production-privilege-logs/{log}/revisions/{revision}/rows", "replace_production_privilege_rows", GeneratedClientTransport},
	{"resolveProductionSelection", methodPOST, "/api/v1/productions/sets/{set_id}/revisions/{revision}/resolve", "resolve_production_selection", GeneratedClientTransport},
	{"reviewProductionMember", methodPOST, "/api/v1/productions/sets/{set_id}/revisions/{revision}/members/{member_id}/review", "review_production_member", GeneratedClientTransport},
	{"sealProductionMembership", methodPOST, "/api/v1/productions/sets/{set_id}/revisions/{revision}/seal", "seal_production_membership", GeneratedClientTransport},
	{"selectProductionGateAuthority", methodPUT, "/api/v1/productions/sets/{set_id}/revisions/{revision}/gate-authority", "select_production_gate_authority", GeneratedClientTransport},
	{"validateProductionPrivilegeLog", methodPOST, "/api/v1/production-privilege-logs/{log}/revisions/{revision}/validate", "validate_production_privilege_log", GeneratedClientTransport},
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
