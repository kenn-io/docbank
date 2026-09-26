import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { sessionResponse } from "../api-transport.js";
import { digestHeaderMatches, readExactBody } from "../download.js";
import { getProductionDraftAt, type ProductionMember } from "./api.js";

const maxEditorPDFBytes = 64 * 1024 * 1024;
const shaPattern = /^[0-9a-f]{64}$/;

// EmbedPDF receives only bytes that match the retained member and the draft
// that was selected. The source version may be an email or another non-PDF.
export async function loadProductionSourcePDF(session: string, setID: string, revision: number,
  etag: number, member: ProductionMember, signal: AbortSignal): Promise<Uint8Array<ArrayBuffer>> {
  if (!shaPattern.test(member.pdf_sha256) || !Number.isSafeInteger(member.pdf_size) ||
      member.pdf_size < 1 || member.pdf_size > maxEditorPDFBytes || !member.id) {
    throw new Error("The selected member has no supported retained PDF.");
  }
  const url = `/api/v1/productions/sets/${encodeURIComponent(setID)}/revisions/${revision}` +
    `/members/${encodeURIComponent(member.id)}/pdf`;
  const response = await sessionResponse<Response>(url, { session, signal, headers: { "If-Match": String(etag), Accept: "application/pdf" } });
  const headers = response.headers;
  if (headers.get("Content-Type")?.split(";", 1)[0] !== "application/pdf" ||
      headers.get("X-Docbank-Blob-Hash") !== member.pdf_sha256 ||
      headers.get("X-Docbank-Blob-Size") !== String(member.pdf_size)) {
    throw new Error("The received PDF disagreed with the selected production member.");
  }
  const bytes = await readExactBody(response, member.pdf_size, signal, "The received production PDF has the wrong size.");
  const actual = bytesToHex(sha256(bytes));
  if (actual !== member.pdf_sha256 || !digestHeaderMatches(headers, actual)) {
    throw new Error("The received PDF disagreed with the selected production member.");
  }
  const current = await getProductionDraftAt(session, setID, revision, signal);
  signal.throwIfAborted();
  if (current.set_id !== setID || current.revision !== revision || current.etag !== etag || current.state !== "draft") {
    throw new Error("The production draft changed while loading the source PDF.");
  }
  return bytes;
}
