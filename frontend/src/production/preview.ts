import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { sessionResponse } from "../api-transport.js";
import { digestHeaderMatches, readExactBody } from "../download.js";
import { createProductionPreview, getProductionDraftAt, type ProductionPreviewTicket } from "./api.js";

const shaPattern = /^[0-9a-f]{64}$/;
const ticketURLPattern = /^\/api\/daemon\/web-download\/file\?ticket=[A-Za-z0-9_-]{43}$/;
const maxImageBytes = 32 * 1024 * 1024;
const maxTextBytes = 16 * 1024 * 1024;

export interface VerifiedProductionPreview {
  image: Uint8Array<ArrayBuffer>;
  text: string;
  previewInputSHA256: string;
  resolvedSHA256: string;
}

export class StaleProductionPreviewError extends Error {
  constructor() { super("The production draft changed while loading the preview."); }
}

function validArtifact(value: unknown, maxSize: number): value is ProductionPreviewTicket["image"] {
  if (!value || typeof value !== "object") return false;
  const artifact = value as Partial<ProductionPreviewTicket["image"]>;
  return typeof artifact.url === "string" && ticketURLPattern.test(artifact.url) &&
    typeof artifact.sha256 === "string" && shaPattern.test(artifact.sha256) &&
    typeof artifact.size === "number" && Number.isSafeInteger(artifact.size) &&
    artifact.size > 0 && artifact.size <= maxSize;
}

async function downloadArtifact(session: string, artifact: ProductionPreviewTicket["image"],
  mediaType: "image/png" | "text/plain", signal: AbortSignal): Promise<Uint8Array<ArrayBuffer>> {
  const response = await sessionResponse<Response>(artifact.url, { session, signal, headers: { Accept: mediaType } });
  const headers = response.headers;
  if (headers.get("Content-Type")?.split(";", 1)[0] !== mediaType ||
      headers.get("Content-Length") !== String(artifact.size) ||
      headers.get("X-Docbank-Blob-Hash") !== artifact.sha256 ||
      headers.get("X-Docbank-Blob-Size") !== String(artifact.size)) {
    throw new Error(`The production preview ${mediaType === "image/png" ? "image" : "text"} disagreed with its verified ticket.`);
  }
  const bytes = await readExactBody(response, artifact.size, signal,
    "The received production preview has the wrong size.");
  if (bytesToHex(sha256(bytes)) !== artifact.sha256 || !digestHeaderMatches(headers, artifact.sha256)) {
    throw new Error(`The production preview ${mediaType === "image/png" ? "image" : "text"} disagreed with its verified ticket.`);
  }
  return bytes;
}

/** Consume the preview's one-use tickets before exposing any output to the page. */
export async function loadProductionPreview(session: string, setID: string, revision: number, etag: number,
  memberID: string, page: number, operationID: string, signal: AbortSignal): Promise<VerifiedProductionPreview> {
  const ticket = await createProductionPreview(session, setID, revision, etag, memberID, page, operationID, signal);
  if (!ticket || ticket.operation_id !== operationID || !shaPattern.test(ticket.preview_input_sha256) ||
      !shaPattern.test(ticket.resolved_sha256) || !validArtifact(ticket.image, maxImageBytes) ||
      !validArtifact(ticket.text, maxTextBytes) || ticket.image.url === ticket.text.url) {
    throw new Error("The production preview ticket disagreed with the requested operation or contains an invalid URL.");
  }
  const image = await downloadArtifact(session, ticket.image, "image/png", signal);
  const text = await downloadArtifact(session, ticket.text, "text/plain", signal);
  const current = await getProductionDraftAt(session, setID, revision, signal);
  signal.throwIfAborted();
  if (current.set_id !== setID || current.revision !== revision || current.etag !== etag || current.state !== "draft") {
    throw new StaleProductionPreviewError();
  }
  return { image, text: new TextDecoder("utf-8", { fatal: true }).decode(text),
    previewInputSHA256: ticket.preview_input_sha256, resolvedSHA256: ticket.resolved_sha256 };
}
