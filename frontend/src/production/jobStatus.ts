import { getProductionJobStatus, type ProductionJobStatus } from "./api.js";

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const shaPattern = /^[0-9a-f]{64}$/;
const states = new Set(["queued", "running", "succeeded", "failed", "canceled"]);

export function validProductionJobID(value: string): boolean { return uuidPattern.test(value); }

/** Reject a status that cannot be tied to the selected set and exact job. */
export async function loadProductionJobStatus(session: string, setID: string, jobID: string,
  signal: AbortSignal): Promise<ProductionJobStatus> {
  if (!validProductionJobID(jobID)) throw new Error("Enter a valid production job ID.");
  const status = await getProductionJobStatus(session, setID, jobID, signal);
  signal.throwIfAborted();
  if (!status || status.set_id !== setID || status.job_id !== jobID ||
      !Number.isSafeInteger(status.revision) || status.revision < 1 ||
      !states.has(status.state) || !shaPattern.test(status.revision_sha256) ||
      (status.state === "succeeded" ? !shaPattern.test(status.receipt_sha256 ?? "") : !!status.receipt_sha256)) {
    throw new Error("The production job status disagreed with the selected set or job.");
  }
  return status;
}
