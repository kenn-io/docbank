import { finalizeProductionDraft, getProductionDraftAt, getProductionSet,
  type ProductionDraft, type ProductionFinalizationResult, type ProductionSet } from "./api.js";

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const shaPattern = /^[0-9a-f]{64}$/;

export class StaleProductionDraftError extends Error {}

export interface FinalizeAttempt {
  set_id: string;
  revision: number;
  etag: number;
  namespace_id: string;
  snapshot_id: string;
  operation_id: string;
}

/** Finalize current numbered authority, or replay the same frozen command after response loss. */
export async function finalizeCurrentProductionDraft(session: string, set: ProductionSet, draft: ProductionDraft,
  attempt: FinalizeAttempt, signal: AbortSignal, replay = false): Promise<ProductionFinalizationResult> {
  if (draft.state !== "draft" || !draft.membership_sealed || draft.numbering_recipe_id !== "bates-sequential-v1" ||
      draft.set_id !== set.id || draft.revision !== set.head_revision || draft.etag < 1 ||
      attempt.set_id !== draft.set_id || attempt.revision !== draft.revision || attempt.etag !== draft.etag ||
      !uuidPattern.test(attempt.namespace_id) || !uuidPattern.test(attempt.snapshot_id) ||
      !uuidPattern.test(attempt.operation_id)) {
    throw new Error("Choose a sealed, numbered production draft before finalization.");
  }
  if (!replay) {
    const currentSet = await getProductionSet(session, set.id, signal);
    signal.throwIfAborted();
    if (currentSet.id !== set.id || currentSet.head_revision !== draft.revision) {
      throw new StaleProductionDraftError("The production set changed. Refresh before finalization.");
    }
    const currentDraft = await getProductionDraftAt(session, set.id, draft.revision, signal);
    signal.throwIfAborted();
    if (currentDraft.set_id !== draft.set_id || currentDraft.revision !== draft.revision ||
        currentDraft.etag !== draft.etag || currentDraft.state !== "draft" ||
        !currentDraft.membership_sealed || currentDraft.member_hash !== draft.member_hash ||
        currentDraft.numbering_recipe_id !== draft.numbering_recipe_id) {
      throw new StaleProductionDraftError("The production draft changed. Refresh before finalization.");
    }
  }
  const result = await finalizeProductionDraft(session, attempt.set_id, attempt.revision, attempt.etag,
    attempt.operation_id, attempt.namespace_id, attempt.snapshot_id, signal);
  signal.throwIfAborted();
  if (result.operation_id !== attempt.operation_id || result.namespace_id !== attempt.namespace_id ||
      result.snapshot_id !== attempt.snapshot_id || result.draft.set_id !== attempt.set_id ||
      result.draft.revision !== attempt.revision || result.draft.etag !== attempt.etag ||
      result.draft.state !== "finalized" || !shaPattern.test(result.prepared_sha256) ||
      !shaPattern.test(result.receipt_sha256)) {
    throw new Error("The finalization receipt disagreed with the selected draft.");
  }
  return result;
}
