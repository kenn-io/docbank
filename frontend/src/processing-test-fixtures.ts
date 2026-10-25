import type { ProcessingFlowHop, ProcessingPlan } from "./generated/docbank.js";

export const versionID = "11111111-1111-4111-8111-111111111111";

export function hop(overrides: Partial<ProcessingFlowHop> = {}): ProcessingFlowHop {
  return {
    capability: "rendition",
    provider_id: "plaintext.in-process-v1",
    trust_boundary: "local_process",
    input_classes: ["original_file"],
    disclose_filename: false,
    runtime_disclosure: {
      immediate_processor: "plaintext.in-process-v1",
      ultimate_processor: "plaintext.in-process-v1",
      endpoint: "in-process",
      deployment: "d".repeat(64),
      metadata_classes: ["byte_length", "content_hash"],
      retained_artifact_roles: ["normalized_evidence", "sanitized_markdown"],
    },
    ...overrides,
  };
}

export const doclingHop = hop({
  provider_id: "docling.serve-v1",
  trust_boundary: "hosted_provider",
  input_classes: ["original_file", "filename"],
  disclose_filename: true,
  filename: "report.txt",
  runtime_disclosure: {
    immediate_processor: "docbank-docling-document/v1",
    ultimate_processor: "docling.serve-v1",
    endpoint: "https://docling.example.com",
    deployment: "2".repeat(64),
    metadata_classes: ["byte_length", "sanitized_filename"],
    retained_artifact_roles: ["provider_markdown", "sanitized_markdown"],
  },
});

export const embeddingHops = [
  hop({
    capability: "embedding",
    provider_id: "openai-compatible.embeddings-v1",
    trust_boundary: "operator_network",
    input_classes: ["rendition_chunk"],
    runtime_disclosure: {
      immediate_processor: "openai-compatible.embeddings-v1",
      ultimate_processor: "openai-compatible.embeddings-v1",
      endpoint: "http://127.0.0.1:9999",
      deployment: "e".repeat(64),
      model: "synthetic-model",
      model_revision: "v1",
      metadata_classes: ["byte_length"],
      retained_artifact_roles: [],
    },
  }),
  hop({
    capability: "query_embedding",
    provider_id: "openai-compatible.embeddings-v1",
    trust_boundary: "operator_network",
    input_classes: ["query_text"],
    runtime_disclosure: {
      immediate_processor: "openai-compatible.embeddings-v1",
      ultimate_processor: "openai-compatible.embeddings-v1",
      endpoint: "http://127.0.0.1:9999",
      deployment: "e".repeat(64),
      metadata_classes: ["query_role"],
      retained_artifact_roles: [],
    },
  }),
];

export function plan(profile: string, overrides: Partial<ProcessingPlan> = {}): ProcessingPlan {
  return {
    fingerprint: "f".repeat(64),
    vault_uid: "22222222-2222-4222-8222-222222222222",
    selector: { node_id: 42, content_version_id: versionID, profile },
    profile_fingerprint: "a".repeat(64),
    flow: [hop()],
    disclosed_classes: ["original_file"],
    retained_classes: ["sanitized_markdown", "normalized_evidence"],
    estimate: { source_bytes: 64, provider_calls: 1, vector_spaces: 0 },
    consent_required: true,
    consent_state: "required",
    backup_consequence: "retained derivatives are included in catalog-authorized backups",
    ...overrides,
  };
}
