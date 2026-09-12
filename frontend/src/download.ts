import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { requestResponse, type ContentVersion, type Node } from "./api.js";
import { validatePDFReceipt, type EmailPDFReceipt } from "./email-pdf.js";
import type { SelectedSource } from "./selectedSource.js";

export interface DownloadProgress {
  received: number;
  total: number;
}

export interface PreparedDownload {
  url: string;
  name: string;
  versionID: string;
  blobHash: string;
  size: number;
}

interface DownloadEvent {
  phase?: string;
  received?: number;
  total?: number;
  url?: string;
  name?: string;
  version_id?: string;
  blob_hash?: string;
  detail?: string;
}

interface DownloadAuthority {
	PDFProfile?: string;
	PDFAttachment?: string;
  nodeID: number;
  revision: number;
  name?: string;
  versionID: string;
  blobHash: string;
  size: number;
}

export async function prepareEmailPDFDownload(session: string, node: Node, version: ContentVersion, receipt: EmailPDFReceipt, signal: AbortSignal, onprogress: (progress: DownloadProgress) => void): Promise<PreparedDownload> {
  validatePDFReceipt(receipt, version.id);
  if (receipt.source.node_id !== node.id || version.node_id !== node.id || receipt.source.sha256 !== version.blob_hash || receipt.source.size !== version.size) throw new Error("The PDF source disagrees with the selected email.");
  return prepareDownload(session, { nodeID: node.id, revision: node.revision, name: "message.pdf", versionID: version.id, blobHash: receipt.output.pdf_sha256, size: receipt.output.pdf_size, PDFProfile: receipt.profile_fingerprint, PDFAttachment: receipt.attachment_id }, signal, onprogress, "native");
}
export type DownloadPurpose = "native" | "preview";
export type VerifiedPreview =
  | { kind: "text"; text: string; mediaType: string }
  | { kind: "image"; url: string; mediaType: string };

const previewTextMaxBytes = 16 * 1024 * 1024;
const previewImageMaxBytes = 32 * 1024 * 1024;

export async function prepareCurrentDownload(
  session: string,
  node: Node,
  signal: AbortSignal,
  onprogress: (progress: DownloadProgress) => void,
): Promise<PreparedDownload> {
  if (
    node.kind !== "file" ||
    !node.current_version_id ||
    !node.blob_hash ||
    node.revision < 1 ||
    node.size < 0
  ) {
    throw new Error("The selected document does not have complete download authority.");
  }
  return prepareDownload(
    session,
    {
      nodeID: node.id,
      revision: node.revision,
      name: node.name,
      versionID: node.current_version_id,
      blobHash: node.blob_hash,
      size: node.size,
    },
    signal,
    onprogress,
    "native",
  );
}

export async function prepareVersionDownload(
  session: string,
  node: Node,
  version: ContentVersion,
  signal: AbortSignal,
  onprogress: (progress: DownloadProgress) => void,
): Promise<PreparedDownload> {
  if (
    node.kind !== "file" ||
    node.revision < 1 ||
    version.node_id !== node.id ||
    !version.id ||
    !version.blob_hash ||
    version.size < 0
  ) {
    throw new Error("The selected version does not have complete download authority.");
  }
  return prepareDownload(
    session,
    {
      nodeID: node.id,
      revision: node.revision,
      name: node.name,
      versionID: version.id,
      blobHash: version.blob_hash,
      size: version.size,
    },
    signal,
    onprogress,
    "native",
  );
}

export async function prepareExactDownload(
  session: string,
  source: SelectedSource,
  authorizationRevision: number,
  purpose: DownloadPurpose,
  signal: AbortSignal,
  onprogress: (progress: DownloadProgress) => void,
): Promise<PreparedDownload> {
  if (
    source.nodeID < 1 ||
    authorizationRevision < 1 ||
    !source.versionID ||
    !/^[0-9a-f]{64}$/.test(source.blobHash) ||
    source.size < 0 ||
    !Number.isSafeInteger(source.size)
  ) {
    throw new Error(
      "The selected source does not have complete download authority.",
    );
  }
  return prepareDownload(
    session,
    {
      nodeID: source.nodeID,
      revision: authorizationRevision,
      versionID: source.versionID,
      blobHash: source.blobHash,
      size: source.size,
    },
    signal,
    onprogress,
    purpose,
  );
}

async function prepareDownload(
  session: string,
  authority: DownloadAuthority,
  signal: AbortSignal,
  onprogress: (progress: DownloadProgress) => void,
  purpose: DownloadPurpose,
): Promise<PreparedDownload> {
  const response = await requestResponse("/api/daemon/web-download", session, {
    method: "POST",
    headers: {
      Accept: "application/x-ndjson",
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      node_id: authority.nodeID,
      revision: authority.revision,
      version_id: authority.versionID,
      blob_hash: authority.blobHash,
      size: authority.size,
      email_pdf_profile: authority.PDFProfile,
      email_pdf_attachment: authority.PDFAttachment,
      ...(purpose === "preview" ? { purpose: "preview" } : {}),
    }),
    signal,
  });
  if (!response.body) throw new Error("The download response did not contain a progress stream.");

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffered = "";
  let received = 0;
  let ready: PreparedDownload | undefined;

  try {
    while (true) {
      const result = await reader.read();
      buffered += decoder.decode(result.value, { stream: !result.done });
      if (buffered.length > 64 * 1024) {
        throw new Error("The download progress response was unexpectedly large.");
      }
      const lines = buffered.split("\n");
      buffered = lines.pop() ?? "";
      for (const line of lines) {
        if (!line.trim()) continue;
        if (ready) throw new Error("The download response continued after publication.");
        const event = JSON.parse(line) as DownloadEvent;
        if (event.phase === "error") {
          throw new Error(event.detail || "Docbank could not verify this document.");
        }
        if (event.phase === "progress") {
          const progress = validateProgress(event, authority.size, received);
          received = progress.received;
          onprogress(progress);
          continue;
        }
        if (event.phase === "ready") {
          ready = validateReady(event, authority);
          onprogress({ received: ready.size, total: ready.size });
          continue;
        }
        throw new Error("The download response contained an unknown progress event.");
      }
      if (result.done) break;
    }
    if (buffered.trim()) {
      const event = JSON.parse(buffered) as DownloadEvent;
      if (event.phase !== "ready" || ready) {
        throw new Error("The download response ended with an invalid progress event.");
      }
      ready = validateReady(event, authority);
      onprogress({ received: ready.size, total: ready.size });
    }
  } catch (cause) {
    await reader.cancel().catch(() => undefined);
    if (ready) await cancelPreparedDownload(session, ready).catch(() => undefined);
    throw cause;
  }
  if (!ready) throw new Error("The download ended before Docbank published verified bytes.");
  if (signal.aborted) {
    await cancelPreparedDownload(session, ready).catch(() => undefined);
    signal.throwIfAborted();
  }
  return ready;
}

export async function cancelPreparedDownload(
  session: string,
  download: PreparedDownload,
): Promise<void> {
  const prefix = "/api/daemon/web-download/file?ticket=";
  if (!download.url.startsWith(prefix)) return;
  const ticket = download.url.slice(prefix.length);
  if (!ticket || ticket.includes("&")) return;
  await requestResponse(
    `/api/daemon/web-download?ticket=${encodeURIComponent(ticket)}`,
    session,
    { method: "DELETE" },
  );
}

export async function readVerifiedPreview(
  session: string,
  source: SelectedSource,
  authorizationRevision: number,
  signal: AbortSignal,
  onprogress: (progress: DownloadProgress) => void,
): Promise<VerifiedPreview> {
  const eligibility = previewEligibility(source.mimeType, source.size);
  let prepared: PreparedDownload | undefined;
  try {
    prepared = await prepareExactDownload(
      session,
      source,
      authorizationRevision,
      "preview",
      signal,
      onprogress,
    );
    const response = await requestResponse(prepared.url, session, {
      headers: { Accept: source.mimeType || "application/octet-stream" },
      signal,
    });
    validatePreviewHeaders(response.headers, source, eligibility.mediaType);
    const bytes = await readExactBody(response, source.size);
    const computedHash = bytesToHex(sha256(bytes));
    if (
      computedHash !== source.blobHash ||
      !digestHeaderMatches(response.headers, computedHash)
    ) {
      throw new Error(
        "The received preview disagreed with the selected document.",
      );
    }
    if (eligibility.kind === "text") {
      let text: string;
      try {
        text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
      } catch {
        throw new Error("The selected text preview is not valid UTF-8.");
      }
      return { kind: "text", text, mediaType: eligibility.mediaType };
    }
    const url = URL.createObjectURL(
      new Blob([Uint8Array.from(bytes).buffer], {
        type: eligibility.mediaType,
      }),
    );
    return { kind: "image", url, mediaType: eligibility.mediaType };
  } catch (cause) {
    if (prepared)
      await cancelPreparedDownload(session, prepared).catch(() => undefined);
    throw cause;
  }
}

export function previewEligibility(
  rawMediaType: string,
  size: number,
): {
  kind: "text" | "image";
  mediaType: string;
} {
  const segments = rawMediaType.split(";");
  const mediaType = (segments.shift() ?? "").trim().toLowerCase();
  if (!/^[a-z0-9!#$&^_.+-]+\/[a-z0-9!#$&^_.+-]+$/.test(mediaType)) {
    throw new Error(
      "The selected version does not have an eligible preview media type.",
    );
  }
  let charset = "";
  for (const segment of segments) {
    const match = /^\s*charset\s*=\s*(?:"([^"]+)"|([^\s;]+))\s*$/i.exec(
      segment,
    );
    if (!match || charset) {
      throw new Error(
        "The selected version does not have eligible preview media parameters.",
      );
    }
    charset = (match[1] ?? match[2] ?? "").toLowerCase();
  }
  const text =
    (mediaType.startsWith("text/") && mediaType !== "text/html") ||
    mediaType === "application/json" ||
    mediaType === "application/x-ndjson";
  if (text) {
    if (charset && charset !== "utf-8" && charset !== "us-ascii") {
      throw new Error(
        "The selected text version uses an unsupported character set.",
      );
    }
    if (size > previewTextMaxBytes) {
      throw new Error(
        "Text previews are limited to 16 MiB; use verified download instead.",
      );
    }
    return { kind: "text", mediaType };
  }
  const image =
    charset === "" && ["image/png", "image/jpeg"].includes(mediaType);
  if (image) {
    if (size > previewImageMaxBytes) {
      throw new Error(
        "Image previews are limited to 32 MiB; use verified download instead.",
      );
    }
    return { kind: "image", mediaType };
  }
  throw new Error(
    "The selected version is not an eligible text or raster image preview.",
  );
}

function validatePreviewHeaders(
  headers: Headers,
  source: SelectedSource,
  mediaType: string,
): void {
  const receivedType = headers
    .get("Content-Type")
    ?.split(";", 1)[0]
    ?.trim()
    .toLowerCase();
  if (
    headers.get("X-Docbank-Content-Version") !== source.versionID ||
    headers.get("X-Docbank-Blob-Hash") !== source.blobHash ||
    headers.get("X-Docbank-Blob-Size") !== String(source.size) ||
    headers.get("Content-Length") !== String(source.size) ||
    receivedType !== mediaType
  ) {
    throw new Error(
      "The received preview disagreed with the selected document.",
    );
  }
}

async function readExactBody(
  response: Response,
  expectedSize: number,
): Promise<Uint8Array> {
  if (!response.body)
    throw new Error("The preview response did not contain document bytes.");
  const bytes = new Uint8Array(expectedSize);
  const reader = response.body.getReader();
  let received = 0;
  try {
    while (true) {
      const result = await reader.read();
      if (result.done) break;
      if (received + result.value.length > expectedSize) {
        throw new Error(
          "The received preview disagreed with the selected document.",
        );
      }
      bytes.set(result.value, received);
      received += result.value.length;
    }
  } catch (cause) {
    await reader.cancel().catch(() => undefined);
    throw cause;
  }
  if (received !== expectedSize) {
    throw new Error(
      "The received preview disagreed with the selected document.",
    );
  }
  return bytes;
}

function digestHeaderMatches(headers: Headers, expectedHash: string): boolean {
  const match = /^sha-256=:([A-Za-z0-9+/]+={0,2}):$/.exec(
    headers.get("Content-Digest") ?? "",
  );
  if (!match) return false;
  try {
    const decoded = atob(match[1] ?? "");
    return [...decoded]
      .map((unit) => unit.charCodeAt(0).toString(16).padStart(2, "0"))
      .join("") === expectedHash;
  } catch {
    return false;
  }
}

function validateProgress(
  event: DownloadEvent,
  expectedTotal: number,
  previous: number,
): DownloadProgress {
  const received = event.received ?? 0;
  const total = event.total;
  if (
    !Number.isSafeInteger(received) ||
    !Number.isSafeInteger(total) ||
    total !== expectedTotal ||
    received < previous ||
    received < 0 ||
    received > total
  ) {
    throw new Error("The download progress disagreed with document authority.");
  }
  return { received, total };
}

function validateReady(event: DownloadEvent, authority: DownloadAuthority): PreparedDownload {
  const name = event.name;
  const versionID = event.version_id;
  const blobHash = event.blob_hash;
  const url = event.url;
  if (
    (event.received ?? 0) !== authority.size ||
    event.total !== authority.size ||
    typeof name !== "string" ||
    typeof versionID !== "string" ||
    typeof blobHash !== "string" ||
    typeof url !== "string" ||
    (authority.name !== undefined && name !== authority.name) ||
    versionID !== authority.versionID ||
    blobHash !== authority.blobHash ||
    !url?.startsWith("/api/daemon/web-download/file?ticket=")
  ) {
    throw new Error("The verified download disagreed with the selected document.");
  }
  return {
    url,
    name,
    versionID,
    blobHash,
    size: event.total,
  };
}

export function offerPreparedDownload(download: PreparedDownload): void {
  const link = document.createElement("a");
  link.href = download.url;
  link.download = download.name;
  link.rel = "noreferrer";
  link.hidden = true;
  document.body.append(link);
  link.click();
  link.remove();
}
