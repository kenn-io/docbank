package bundle

const InspectionPageSize = 50
const MaxOutputProblems = MaxDocumentRows*6 + MaxMembers*2

type EmailPDFRecipeChoice struct {
	RecipeSHA256    string `json:"recipe_sha256"`
	Paper           string `json:"paper"`
	RendererVersion string `json:"renderer_version"`
	Messages        int    `json:"messages"`
	Ambiguous       int    `json:"ambiguous"`
}

type EmailPDFRecipes struct {
	SourceID   string                 `json:"source_id"`
	MemberHash string                 `json:"member_hash"`
	Total      int                    `json:"total"`
	Recipes    []EmailPDFRecipeChoice `json:"recipes"`
}

type OutputProblem struct {
	NodeID    int64  `json:"node_id"`
	VersionID string `json:"version_id"`
	PartPath  string `json:"part_path,omitzero"`
	Role      string `json:"role"`
	Reason    string `json:"reason"`
}

type OutputProblems struct {
	PlanID      string          `json:"plan_id"`
	Fingerprint string          `json:"fingerprint"`
	After       int             `json:"after"`
	Next        int             `json:"next"`
	Total       int             `json:"total"`
	Items       []OutputProblem `json:"items"`
}

// AttachmentPublicationChoice describes one candidate when a source member has
// multiple published attachment sets. Discovery never chooses a set implicitly.
type AttachmentPublicationChoice struct {
	NodeID       int64  `json:"node_id"`
	VersionID    string `json:"version_id"`
	Name         string `json:"name"`
	OperationID  string `json:"operation_id"`
	GenerationID string `json:"generation_id"`
	CreatedAt    string `json:"created_at"`
	State        string `json:"state"`
	Attachments  int    `json:"attachments"`
}

type AttachmentPublications struct {
	SourceID   string                        `json:"source_id"`
	MemberHash string                        `json:"member_hash"`
	After      int                           `json:"after"`
	Next       int                           `json:"next"`
	Total      int                           `json:"total"`
	Items      []AttachmentPublicationChoice `json:"items"`
}
