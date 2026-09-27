import { forkProductionDraft, getProductionDraftAt, getProductionSet,
  type ProductionDraft, type ProductionSet } from "./api.js";

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export class StaleProductionForkError extends Error {}

export interface ForkAttempt {
  set_id: string;
  revision: number;
  etag: number;
  operation_id: string;
}

/** Fork one finalized source, or replay its frozen operation after response loss. */
export async function forkCurrentProductionDraft(session: string, set: ProductionSet, draft: ProductionDraft,
  attempt: ForkAttempt, signal: AbortSignal, replay = false): Promise<ProductionDraft> {
  if (draft.state !== "finalized" || draft.set_id !== set.id || draft.revision !== set.head_revision ||
      !Number.isSafeInteger(draft.etag) || draft.etag < 1 ||
      attempt.set_id !== draft.set_id || attempt.revision !== draft.revision || attempt.etag !== draft.etag ||
      !uuidPattern.test(attempt.operation_id)) {
    throw new Error("Choose the current finalized production revision before creating a new draft.");
  }
  if (!replay) {
    const currentSet = await getProductionSet(session, set.id, signal);
    signal.throwIfAborted();
    if (currentSet.id !== set.id || currentSet.head_revision !== draft.revision)
      throw new StaleProductionForkError("The production set changed. Refresh before creating a new draft.");
    const currentDraft = await getProductionDraftAt(session, set.id, draft.revision, signal);
    signal.throwIfAborted();
    if (currentDraft.set_id !== draft.set_id || currentDraft.revision !== draft.revision ||
        currentDraft.etag !== draft.etag || currentDraft.state !== "finalized" ||
        currentDraft.member_hash !== draft.member_hash ||
        currentDraft.numbering_recipe_id !== draft.numbering_recipe_id) {
      throw new StaleProductionForkError("The finalized revision changed. Refresh before creating a new draft.");
    }
  }
  const result = await forkProductionDraft(session, attempt.set_id, attempt.revision, attempt.operation_id, signal);
  signal.throwIfAborted();
  if (result.set_id !== attempt.set_id || !Number.isSafeInteger(result.revision) ||
      result.revision <= attempt.revision || result.etag !== 1 || result.state !== "draft" ||
      result.membership_sealed || result.member_hash !== draft.member_hash ||
      result.numbering_recipe_id !== draft.numbering_recipe_id) {
    throw new Error("The new draft disagreed with the selected finalized revision.");
  }
  return result;
}
