import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { getProductionMapChunk, type ProductionDecision } from "./api.js";

const maxReviewMapBytes = 8 * 1024 * 1024;
const chunkBytes = 65536;
const hashPattern = /^[0-9a-f]{64}$/;

export interface ReviewContext { before: string; selected: string; after: string }

/** Read an exact retained text map; never trust a chunk or viewport excerpt alone. */
export async function loadReviewContext(session: string, setID: string, revision: number, memberID: string,
  selector: ProductionDecision["selector"], signal: AbortSignal): Promise<ReviewContext> {
  if (selector.kind !== "text" || !selector.span || !hashPattern.test(selector.map_sha256))
    throw new Error("This flagged region needs the original source editor before a decision can be made.");

  let cursor = "";
  let received = 0;
  let raw: Uint8Array | undefined;
  const seen = new Set<string>();
  for (;;) {
    const chunk = await getProductionMapChunk(session, setID, revision, memberID, cursor, signal);
    if (chunk.map_sha256 !== selector.map_sha256 || !Number.isSafeInteger(chunk.total_bytes) ||
        chunk.total_bytes < 1 || chunk.total_bytes > maxReviewMapBytes ||
        !Number.isSafeInteger(chunk.offset) || chunk.offset !== received ||
        !hashPattern.test(chunk.chunk_sha256) || typeof chunk.data !== "string" ||
        typeof chunk.next_cursor !== "string" || chunk.next_cursor.length > 512)
      throw new Error("The retained text map changed during review.");
    raw ??= new Uint8Array(chunk.total_bytes);
    if (raw.length !== chunk.total_bytes) throw new Error("The retained text map changed during review.");
    let part: Uint8Array;
    try { part = Uint8Array.from(atob(chunk.data), char => char.charCodeAt(0)); }
    catch { throw new Error("The retained text map chunk is invalid."); }
    if (part.length < 1 || part.length > chunkBytes || received + part.length > raw.length ||
        bytesToHex(sha256(part)) !== chunk.chunk_sha256)
      throw new Error("The retained text map chunk failed verification.");
    raw.set(part, received);
    received += part.length;
    if (!chunk.next_cursor) break;
    if (received === raw.length || seen.has(chunk.next_cursor))
      throw new Error("The retained text map cursor is invalid.");
    seen.add(chunk.next_cursor);
    cursor = chunk.next_cursor;
  }
  if (!raw || received !== raw.length || bytesToHex(sha256(raw)) !== selector.map_sha256)
    throw new Error("The retained text map failed verification.");

  let map: unknown;
  try { map = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(raw)); }
  catch { throw new Error("The retained text map is invalid."); }
  if (!map || typeof map !== "object" || (map as { contract?: unknown }).contract !== "aligned-text/v1" ||
      typeof (map as { text?: unknown }).text !== "string")
    throw new Error("The retained text map is invalid.");
  const text = (map as { text: string }).text;
  const bytes = new TextEncoder().encode(text);
  const { start, end } = selector.span;
  if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end <= start ||
      end > bytes.length || end - start > 2048)
    throw new Error("This text selection needs the source editor before review.");
  let before: string, selected: string, after: string;
  try {
    const decoder = new TextDecoder("utf-8", { fatal: true });
    before = decoder.decode(bytes.subarray(0, start));
    selected = decoder.decode(bytes.subarray(start, end));
    after = decoder.decode(bytes.subarray(end));
  } catch { throw new Error("The selection splits a UTF-8 character."); }
  const prior = before.slice(-160);
  const following = after.slice(0, 160);
  return {
    before: Array.from(prior.charCodeAt(0) >= 0xdc00 && prior.charCodeAt(0) <= 0xdfff ? prior.slice(1) : prior).slice(-80).join(""),
    selected,
    after: Array.from(following.charCodeAt(following.length - 1) >= 0xd800 && following.charCodeAt(following.length - 1) <= 0xdbff ? following.slice(0, -1) : following).slice(0, 80).join(""),
  };
}
