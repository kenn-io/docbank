import type { Frame } from "./embedpdfAdapter.js";
import { utf16ToUTF8Offset } from "./embedpdfAdapter.js";
import { getProductionDraftAt } from "./api.js";
import { loadVerifiedTextMap } from "./reviewContext.js";

export interface SelectableTextPage { text: string; start: number; frame: Frame }

export async function loadSelectableTextPage(session: string, setID: string, revision: number, etag: number,
  memberID: string, mapSHA256: string, frame: Frame, signal: AbortSignal): Promise<SelectableTextPage> {
  const map = await loadVerifiedTextMap(session, setID, revision, memberID, mapSHA256, signal);
  const pages = map.pages;
  if (!Array.isArray(pages)) throw new Error("The retained text map has no page inventory.");
  const page = pages.find(item => item?.number === frame.page);
  if (!page || pages.filter(item => item?.number === frame.page).length !== 1 ||
      page.frame_sha256 !== frame.sha256 || page.width !== frame.width || page.height !== frame.height)
    throw new Error("The retained text map disagrees with the verified page frame.");
  const raw = new TextEncoder().encode(map.text);
  const start = page.span?.start;
  const end = page.span?.end;
  if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end < start ||
      end > raw.length || end - start > 65536)
    throw new Error("This page has too much mapped text to select in the browser.");
  let text: string;
  try { text = new TextDecoder("utf-8", { fatal: true }).decode(raw.subarray(start, end)); }
  catch { throw new Error("The retained page span splits a UTF-8 character."); }
  const fresh = await getProductionDraftAt(session, setID, revision, signal);
  signal.throwIfAborted();
  if (fresh.set_id !== setID || fresh.revision !== revision || fresh.etag !== etag || fresh.state !== "draft")
    throw new Error("The production draft changed while loading text selection.");
  return { text, start, frame };
}

export function textSpanForSelection(page: SelectableTextPage, start: number, end: number): { start: number; end: number } {
  if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end <= start || end > page.text.length)
    throw new Error("Select a nonempty passage on one page.");
  const first = page.start + utf16ToUTF8Offset(page.text, start);
  const last = page.start + utf16ToUTF8Offset(page.text, end);
  if (last - first > 2048) throw new Error("Select at most 2048 UTF-8 bytes.");
  return { start: first, end: last };
}
