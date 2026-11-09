// Package workerprotocol names the bounded durable conversion worker contract.
package workerprotocol

const (
	// Contract identifies authenticated, idempotent uploaded-file processing.
	Contract = "docbank-conversion-worker/v1"
	// ProviderID distinguishes replay-safe worker jobs from native Docling tasks.
	ProviderID = "docbank.conversion-worker-v1"
	// DocumentAdapter identifies the worker's document evidence policy.
	DocumentAdapter = "docbank-worker-document/v1"
	// ASRAdapter identifies the worker's qualified audio transcription policy.
	ASRAdapter = "docbank-worker-asr/v1"
	// ProtocolHeader must be present on every authenticated worker response.
	ProtocolHeader = "X-Docbank-Worker-Protocol"
	// BackendHopHeader prevents recursive forwarding through worker gateways.
	BackendHopHeader = "X-Docbank-Worker-Backend-Hop"
	// SourceSHA256Header binds accepted bytes to the client-verified digest.
	SourceSHA256Header = "X-Docbank-Source-SHA256"
	// SubmitPath accepts exactly one fixed-format multipart source.
	SubmitPath = "/v1/convert/file/async"
	// PollPath prefixes the issued task identifier.
	PollPath = "/v1/status/poll/"
	// ResultPath prefixes the issued task identifier for repeatable retrieval.
	ResultPath = "/v1/result/"
	// LookupPath recovers an accepted task before new-work readiness checks.
	LookupPath = "/v1/jobs/by-key/"
	// ReadyPath reports readiness for previously unseen request keys.
	ReadyPath = "/ready"
)
