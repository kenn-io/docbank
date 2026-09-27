import { admitProductionJob, getProductionDraftAt, getProductionSet,
  type ProductionDraft, type ProductionJobStatus, type ProductionSet } from "./api.js";

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const shaPattern = /^[0-9a-f]{64}$/;
const states = new Set(["queued", "running", "succeeded", "failed", "canceled"]);

/** Admit only the current finalized authority and check the returned job identity. */
export async function admitCurrentProductionJob(session: string, set: ProductionSet, draft: ProductionDraft,
  jobID: string, operationID: string, signal: AbortSignal): Promise<ProductionJobStatus> {
  if (draft.state !== "finalized" || !draft.membership_sealed || draft.set_id !== set.id ||
      draft.revision !== set.head_revision || !Number.isSafeInteger(draft.etag) || draft.etag < 1 ||
      !uuidPattern.test(jobID) || !uuidPattern.test(operationID)) {
    throw new Error("Choose a finalized production revision before starting a job.");
  }
  const currentSet = await getProductionSet(session, set.id, signal);
  signal.throwIfAborted();
  if (currentSet.id !== set.id || currentSet.head_revision !== draft.revision) {
    throw new Error("The production set changed. Refresh before starting a job.");
  }
  const currentDraft = await getProductionDraftAt(session, set.id, draft.revision, signal);
  signal.throwIfAborted();
  if (currentDraft.set_id !== draft.set_id || currentDraft.revision !== draft.revision ||
      currentDraft.etag !== draft.etag || currentDraft.state !== "finalized" || !currentDraft.membership_sealed) {
    throw new Error("The finalized revision changed. Refresh before starting a job.");
  }
  const status = await admitProductionJob(session, set.id, draft.revision, draft.etag, jobID, operationID, signal);
  signal.throwIfAborted();
  if (status.job_id !== jobID || status.set_id !== set.id || status.revision !== draft.revision ||
      !states.has(status.state) || !shaPattern.test(status.revision_sha256)) {
    throw new Error("The admitted job disagreed with the finalized revision.");
  }
  return status;
}
