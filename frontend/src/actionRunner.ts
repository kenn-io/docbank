import { APIError, requestJSON } from "./api.js";
import { changeBatchTags, type BatchTagReceipt } from "./batch-tags.js";
import type { ActionJournalAccess, PersistedAction } from "./actionJournal.js";

const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export type ActionProgress = (action: Readonly<PersistedAction>, confirmedReceipt?: BatchTagReceipt) => void;

function publish(onProgress: ActionProgress, action: PersistedAction, confirmedReceipt?: BatchTagReceipt): void {
  try {
    onProgress(action, confirmedReceipt);
  } catch {
    // Rendering progress cannot change whether a durable mutation is sent or recorded.
  }
}

export async function readActionVaultID(session: string): Promise<string> {
  if (typeof session !== "string" || session.length === 0) throw new Error("A fresh authenticated browser session is required.");
  const value: unknown = await requestJSON<unknown>("/api/v1/audit/status", session);
  if (typeof value !== "object" || value === null || Array.isArray(value)) throw new Error("The daemon returned an invalid vault identity.");
  const vaultID = (value as Record<string, unknown>).vault_id;
  if (typeof vaultID !== "string" || !uuidV4.test(vaultID)) throw new Error("The daemon returned an invalid vault identity.");
  return vaultID;
}

export async function runAction(
  session: string,
  journal: ActionJournalAccess,
  signal: AbortSignal,
  onProgress: ActionProgress,
): Promise<PersistedAction> {
  let action = await journal.load();
  if (!action) throw new Error("No recoverable action is available.");
  if (!action.checkpoint_verified) throw new Error("Select and verify the saved recovery checkpoint before mutation.");
  if (!journal.consumeResumeConfirmation(action.action_id)) throw new Error("Explicitly confirm this action with the fresh session before mutation.");
  const vaultID = await readActionVaultID(session);
  if (vaultID !== action.vault_id) throw new Error("The recovery action belongs to a different vault.");
  if (action.state === "stale") throw new Error("The action is stale and cannot be resumed without changing its original inputs.");

  async function refreshBatch(current: PersistedAction, index: number): Promise<PersistedAction> {
    const { batch, ...progress } = await journal.loadBatch(index);
    if (progress.action_id !== current.action_id) throw new Error("The retained action changed. Open and confirm it again.");
    return { ...current, ...progress, batches: current.batches.map((item) => item.index === index ? batch : item) };
  }

  for (const planned of action.batches) {
    action = await refreshBatch(action, planned.index);
    const batch = action.batches[planned.index];
    if (signal.aborted || action.state === "paused" || action.state === "stale" || action.state === "complete") break;
    if (batch.receipt || batch.state === "complete") continue;

    await journal.markSending(batch.index);
    action = await refreshBatch(action, batch.index);
    publish(onProgress, action);
    try {
      const receipt = await changeBatchTags(session, batch.request as Parameters<typeof changeBatchTags>[1]);
      await journal.recordReceipt(batch.index, receipt);
      action = await refreshBatch(action, batch.index);
      publish(onProgress, action, receipt);
    } catch (error) {
      if (error instanceof APIError && (
        (error.status === 412 && error.code === "stale_revision") ||
        (error.status === 404 && error.code === "not_found")
      )) {
        await journal.markStale(batch.index);
      } else {
        try {
          await journal.markUncertain(batch.index);
        } catch {
          // The durable sending marker still recovers as uncertain on the next load.
        }
      }
      action = await refreshBatch(action, batch.index);
      publish(onProgress, action);
      throw error;
    }
  }
  return (await journal.load()) ?? action;
}
