import { getProductionDraftAt, resolveProductionPage } from "./api.js";
import type { Frame } from "./embedpdfAdapter.js";

const shaPattern = /^[0-9a-f]{64}$/;

/** Read the current resolved page frame without loading the complete text map. */
export async function loadSelectionFrame(session: string, setID: string, revision: number,
  etag: number, memberID: string, mapSHA256: string, page: number, signal: AbortSignal): Promise<Frame> {
  if (!shaPattern.test(mapSHA256) || !Number.isSafeInteger(page) || page < 1)
    throw new Error("The selected member has no verified page frame.");
  const resolved = await resolveProductionPage(session, setID, revision, etag, memberID, page, signal);
  const frame = resolved.page;
  if (resolved.set_id !== setID || resolved.revision !== revision || resolved.etag !== etag ||
      resolved.member_id !== memberID || resolved.map_sha256 !== mapSHA256 || frame?.number !== page ||
      !shaPattern.test(frame.frame_sha256) || !Number.isSafeInteger(frame.width) || frame.width < 1 ||
      !Number.isSafeInteger(frame.height) || frame.height < 1)
    throw new Error("The resolved page frame disagreed with the selected production member or map.");
  const fresh = await getProductionDraftAt(session, setID, revision, signal);
  signal.throwIfAborted();
  if (fresh.set_id !== setID || fresh.revision !== revision || fresh.etag !== etag || fresh.state !== "draft")
    throw new Error("The production draft changed while loading the page frame.");
  return { page, sha256: frame.frame_sha256, width: frame.width, height: frame.height };
}
