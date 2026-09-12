// Inert-template, allowlist, quote-folding and opaque-frame patterns adapted
// from kenn-io/msgvault 98ef7c85e4f8585be35759594d119c70faeca4e3.
// Copyright (c) 2025-2026 Wes McKinney. MIT; see email-viewer.LICENSE.
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { requestResponse, type ContentVersion } from "./api.js";
import type { SelectedSource } from "./selectedSource.js";
import { pageDigest } from "./pages.js";

const MiB = 1024 * 1024;
export const emailHTMLLimit = 16 * MiB;
const serializedLimit = 32 * MiB;
const imageByteLimit = 5 * MiB;
const imagePixelLimit = 8_000_000;
const aggregatePixelLimit = 32_000_000;
const hashPattern = /^[a-f0-9]{64}$/;
const partPattern = /^[1-9][0-9]*(?:\.[1-9][0-9]*){0,15}$/;
export interface EmailArtifact { role: "body_utf8" | "decoded_payload" | "raw_headers"; sha256: string; size: number }
export interface EmailDiagnostic { code: string; detail: string; path: string | null }
export interface EmailPart {
  path: string; parent_path: string | null; message_path: string;
  media: { declared: string | null; detected: string | null };
  content_id: { value: string | null; state: string }; decode_state: string;
  filename: { safe_name: string }; disposition: string | null;
  payload: EmailArtifact | null; body_utf8: EmailArtifact | null; header_block: EmailArtifact | null;
  diagnostics: EmailDiagnostic[];
}
export interface EmailAlternative { part_path: string; kind: "html" | "plain"; display_state: string; display: EmailArtifact | null }
export interface EmailMessage {
  path: string; selected_body_path: string | null; alternatives: EmailAlternative[];
  fields: Record<string, { header_index: number; state: string; text: string | null }[]>;
  date: { state: string; civil: string | null; timezone_text: string | null; timezone_state: string };
  diagnostics: EmailDiagnostic[];
}
export interface EmailMetadata {
  version: ContentVersion; generation_id: string; attachment_id: string; checksum: string; recipe_fingerprint:string;
  evidence: { contract_version: string; source: { sha256: string; size: number; verification: string };
    recipe:Record<string,unknown>; outcome: string; inventory: { state: string; root_path: string | null; parts: EmailPart[]; messages: EmailMessage[]; diagnostics: EmailDiagnostic[] } | null;
    failure: { detail: string } | null };
}

export async function readBounded(response: Response, maximum: number, signal: AbortSignal): Promise<Uint8Array<ArrayBuffer>> {
  const reader = response.body?.getReader();
  if (!reader) throw new Error("Email response has no stream.");
  const chunks: Uint8Array[] = []; let total = 0;
  const abort = () => { void reader.cancel().catch(() => undefined); };
  signal.addEventListener("abort", abort, { once: true });
  try {
    signal.throwIfAborted();
    for (;;) {
      const { value, done } = await reader.read(); signal.throwIfAborted();
      if (done) break;
      total += value.byteLength;
      if (total > maximum) throw new Error(`Email display byte limit exceeded (${maximum} bytes). Original remains available.`);
      chunks.push(value);
    }
    const bytes = new Uint8Array(total); let offset = 0;
    for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
    return bytes;
  } finally {
    signal.removeEventListener("abort", abort);
    await reader.cancel().catch(() => undefined); reader.releaseLock();
  }
}
function artifactValid(a: EmailArtifact | null): boolean {
  return a === null || !!a && ["body_utf8", "decoded_payload", "raw_headers"].includes(a.role) && hashPattern.test(a.sha256) && Number.isSafeInteger(a.size) && a.size >= 0 && a.size <= 128 * MiB;
}
export function validateEmailMetadata(raw: unknown, source: SelectedSource): EmailMetadata {
  const m = raw as EmailMetadata;
  if (!m?.version || m.version.id !== source.versionID || m.version.node_id !== source.nodeID || m.version.blob_hash !== source.blobHash || m.version.size !== source.size ||
      !hashPattern.test(m.generation_id) || !hashPattern.test(m.attachment_id) || !hashPattern.test(m.checksum) ||
      m.evidence?.contract_version !== "docbank-email/v1" || m.evidence.source?.sha256 !== source.blobHash || m.evidence.source.size !== source.size || m.evidence.source.verification !== "verified") throw new Error("Email metadata disagrees with the exact selected source.");
  // Reuse the existing RFC 8785 digest implementation; derive the DB25
  // generation and attachment with its length-prefixed UTF-8 tuple contract.
  if (!m.evidence.recipe || pageDigest(m.evidence) !== m.checksum || pageDigest(m.evidence.recipe) !== m.recipe_fingerprint ||
      emailTupleDigest("docbank-email-generation/v1",m.evidence.contract_version,source.blobHash,String(source.size),m.recipe_fingerprint,m.checksum) !== m.generation_id ||
      emailTupleDigest("docbank-email-attachment/v1",source.versionID,m.generation_id) !== m.attachment_id) throw new Error("Email canonical evidence or generation binding failed verification.");
  const inv = m.evidence.inventory;
  if (m.evidence.outcome !== "decoded" || !inv) throw new Error(m.evidence.failure?.detail || "Email decoding is unavailable.");
  if (!["complete", "partial"].includes(inv.state) || !Array.isArray(inv.parts) || inv.parts.length > 1000 || !Array.isArray(inv.messages) || inv.messages.length > 1000 || !Array.isArray(inv.diagnostics)) throw new Error("Invalid email inventory.");
  const paths = new Set<string>();
  for (const p of inv.parts) {
    if (!partPattern.test(p.path) || paths.has(p.path) || !partPattern.test(p.message_path) || !p.media || !p.content_id || !p.filename || !Array.isArray(p.diagnostics) ||
        !artifactValid(p.payload) || !artifactValid(p.body_utf8) || !artifactValid(p.header_block) ||
        p.payload && p.payload.role !== "decoded_payload" || p.body_utf8 && p.body_utf8.role !== "body_utf8" || p.header_block && p.header_block.role !== "raw_headers") throw new Error("Invalid email part authority.");
    paths.add(p.path);
    const expected = p.path.includes(".") ? p.path.slice(0, p.path.lastIndexOf(".")) : null;
    if (p.parent_path !== expected) throw new Error("Invalid MIME tree ownership.");
  }
  if (!inv.root_path || !paths.has(inv.root_path)) throw new Error("Missing email root.");
  for (const p of inv.parts) if (p.parent_path && !paths.has(p.parent_path) || !paths.has(p.message_path)) throw new Error("Missing MIME owner.");
  for (const message of inv.messages) {
    if (!paths.has(message.path) || !message.fields || !message.date || !Array.isArray(message.alternatives) || message.alternatives.length > 1000 || !Array.isArray(message.diagnostics)) throw new Error("Invalid email message.");
    for (const alternative of message.alternatives) {
      const p = inv.parts.find((p) => p.path === alternative.part_path);
      if (!p || p.message_path !== message.path || !["html", "plain"].includes(alternative.kind) || !artifactValid(alternative.display) || alternative.display && (alternative.display.role !== "body_utf8" || alternative.display.sha256 !== p.body_utf8?.sha256 || alternative.display.size !== p.body_utf8.size)) throw new Error("Body alternative disagrees with MIME authority.");
    }
  }
  return m;
}
export function emailTupleDigest(...values:string[]):string {
  const digest = sha256.create(); const size = new Uint8Array(8); const view = new DataView(size.buffer);
  for (const value of values) {
    const bytes = new TextEncoder().encode(value); view.setBigUint64(0,BigInt(bytes.length)); digest.update(size); digest.update(bytes);
  }
  return bytesToHex(digest.digest());
}
export async function readEmailMetadata(session: string, source: SelectedSource, signal: AbortSignal): Promise<EmailMetadata> {
  const response = await requestResponse(`/api/v1/versions/${encodeURIComponent(source.versionID)}/email`, session, { signal });
  if (response.status === 202) { await response.body?.cancel(); throw new Error("Email decoding is pending. Process this exact version, then reopen Email."); }
  const raw = await readBounded(response, 16 * MiB, signal); signal.throwIfAborted();
  return validateEmailMetadata(JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(raw)), source);
}
export async function readEmailPart(session: string, metadata: EmailMetadata, part: EmailPart, artifact: EmailArtifact, signal: AbortSignal, maximum = emailHTMLLimit): Promise<Uint8Array<ArrayBuffer>> {
  const owned = metadata.evidence.inventory?.parts.find((p) => p.path === part.path);
  const ref = artifact.role === "body_utf8" ? owned?.body_utf8 : artifact.role === "raw_headers" ? owned?.header_block : owned?.payload;
  if (!ref || ref.sha256 !== artifact.sha256 || ref.size !== artifact.size || artifact.size > maximum) throw new Error(`Email part is unavailable for this display operation (limit ${maximum} bytes).`);
  signal.throwIfAborted();
  const response = await requestResponse(`/api/v1/versions/${metadata.version.id}/email/generations/${metadata.generation_id}/parts/${part.path}/${artifact.role}`, session, { signal, headers: { Accept:"application/octet-stream" } });
  const expected: Record<string,string> = { "X-Docbank-Content-Version": metadata.version.id, "X-Docbank-Email-Generation": metadata.generation_id, "X-Docbank-Email-Attachment": metadata.attachment_id, "X-Docbank-Email-Part-Path":part.path, "X-Docbank-Email-Part-Role":artifact.role, "X-Docbank-Blob-Hash":artifact.sha256, "X-Docbank-Blob-Size":String(artifact.size) };
  if (Object.entries(expected).some(([key,value]) => response.headers.get(key) !== value) || response.headers.get("Content-Type") !== "application/octet-stream") { await response.body?.cancel(); throw new Error("Email part response identity mismatch."); }
  const bytes = await readBounded(response, artifact.size, signal); signal.throwIfAborted();
  const digest = bytesToHex(sha256(bytes)); signal.throwIfAborted();
  if (bytes.byteLength !== artifact.size || digest !== artifact.sha256) throw new Error("Email part SHA-256 or size verification failed.");
  return bytes;
}

const tags = new Set("a abbr address article b blockquote br caption center cite code col colgroup dd del details div dl dt em figcaption figure font footer h1 h2 h3 h4 h5 h6 header hr i img ins kbd li main mark ol p pre q s samp section small span strike strong sub summary sup table tbody td tfoot th thead tr tt u ul var".split(" "));
const discard = new Set("script style form input button select textarea iframe frame frameset object embed svg math template link meta base audio video source track".split(" "));
export interface EmailImage { index: number; cid: string; alt: string }
export function sanitizeEmail(input: string): { html: string; images: EmailImage[]; warnings: string[] } {
  if (new TextEncoder().encode(input).byteLength > emailHTMLLimit) throw new Error("HTML display exceeds 16 MiB; no body was truncated.");
  // Charge markup candidates before the HTML parser can allocate a tree.
  // Literal '<' characters count too: conservative refusal is explicit.
  let markup = 0;
  for (let index = input.indexOf("<"); index !== -1; index = input.indexOf("<",index+1)) {
    if (++markup > 100_000) throw new Error("HTML display exceeds 100,000 markup candidates before parsing; original remains complete.");
  }
  // A template contents-owner document has no browsing context. Sender bytes
  // NEVER enter a live/detached div or a design-detection DOMParser first.
  const template = document.createElement("template"); template.innerHTML = input;
  const output = document.createElement("template"); const images: EmailImage[] = []; const warnings = new Set<string>();
  let count = 0;
  function copy(node: Node, parent: Node, depth: number): void {
    if (++count > 100_000 || depth > 128) throw new Error("HTML display exceeds 100,000 nodes or 128 levels; original remains available without truncation.");
    if (node.nodeType === 3) { parent.appendChild(document.createTextNode(node.textContent ?? "")); return; }
    if (node.nodeType !== 1) return;
    const element = node as HTMLElement; const tag = element.localName;
    if (element.namespaceURI !== "http://www.w3.org/1999/xhtml" || discard.has(tag)) { warnings.add("Active content, forms and external styles were removed."); return; }
    if (!tags.has(tag)) { warnings.add("Unsupported markup was removed."); for (const child of element.childNodes) copy(child,parent,depth+1); return; }
    if (tag === "img") {
      const placeholder = document.createElement("span"); const src = element.getAttribute("src") ?? "";
      const alt = element.getAttribute("alt")?.slice(0,512) || "Inline image";
      placeholder.textContent = `[${alt}: blocked or unavailable image]`;
      if (/^cid:/i.test(src) && images.length < 128) {
        let cid = ""; try { cid = decodeURIComponent(src.slice(4)); } catch { /* explicit placeholder */ }
        if (cid && cid.length <= 998 && !/[\s<>\x00-\x1f]/.test(cid)) { const index = images.length; images.push({ index,cid,alt }); placeholder.setAttribute("data-email-image",String(index)); }
      }
      if (!placeholder.hasAttribute("data-email-image")) warnings.add("Remote, embedded-data or excessive images are blocked. Only verified MIME images can display.");
      parent.appendChild(placeholder); return;
    }
    const clean = document.createElement(tag === "a" ? "span" : tag);
    for (const attribute of element.attributes) {
      const name = attribute.name; const value = attribute.value;
      if (["title","lang"].includes(name)) clean.setAttribute(name,value.slice(0,512));
      else if (name === "dir" && /^(ltr|rtl|auto)$/.test(value)) clean.setAttribute(name,value);
      else if (["colspan","rowspan"].includes(name) && /^(?:[1-9]|[1-9][0-9]|100)$/.test(value)) clean.setAttribute(name,value);
      else warnings.add("Sender links, styling and unsafe attributes are disabled.");
    }
    parent.appendChild(clean);
    for (const child of element.childNodes) copy(child,clean,depth+1);
    if ((tag === "blockquote" || tag === "div" && element.className.includes("gmail_quote")) && (clean.textContent?.length ?? 0) > 300) {
      const details = document.createElement("details"); const summary = document.createElement("summary"); summary.textContent = "Show quoted text";
      clean.replaceWith(details); details.append(summary,clean);
    }
  }
  for (const node of template.content.childNodes) copy(node,output.content,0);
  const html = output.innerHTML;
  if (new TextEncoder().encode(html).byteLength > serializedLimit) throw new Error("Serialized HTML display exceeds 32 MiB; original remains complete.");
  return { html,images,warnings:[...warnings] };
}

export function resolveCID(parts: EmailPart[], bodyPath: string, cid: string): EmailPart | undefined {
  const byPath = new Map(parts.map((p) => [p.path,p])); const body = byPath.get(bodyPath);
  if (!body) return;
  function owner(part: EmailPart): string | undefined {
    let parent = part.parent_path;
    for (let depth = 0; parent && depth < 16; depth++) {
      const p = byPath.get(parent); if (!p || p.message_path !== body!.message_path) return;
      if (p.media.declared === "multipart/related") return p.path;
      parent = p.parent_path;
    }
  }
  const group = owner(body); if (!group) return;
  const found = parts.filter((p) => p.message_path === body.message_path && p.content_id.state === "decoded" && p.content_id.value === cid && owner(p) === group);
  return found.length === 1 ? found[0] : undefined;
}

export function rasterSize(bytes: Uint8Array): { width:number; height:number; mime:string } {
  const view = new DataView(bytes.buffer,bytes.byteOffset,bytes.byteLength); let width = 0; let height = 0; let mime = "";
  if (bytes.length >= 24 && [137,80,78,71,13,10,26,10].every((v,i) => bytes[i] === v) && [73,72,68,82].every((v,i) => bytes[i+12] === v)) {
    width = view.getUint32(16); height = view.getUint32(20); mime = "image/png";
  } else if (bytes[0] === 255 && bytes[1] === 216) {
    for (let offset = 2; offset + 4 < bytes.length;) {
      if (bytes[offset++] !== 255) break;
      while (bytes[offset] === 255) offset++;
      const marker = bytes[offset++]; if (marker === 217 || marker === 218) break;
      const length = offset + 2 <= bytes.length ? view.getUint16(offset) : 0;
      if (length < 2 || offset + length > bytes.length) break;
      if ([192,193,194].includes(marker) && length >= 8) { height = view.getUint16(offset+3); width = view.getUint16(offset+5); mime = "image/jpeg"; break; }
      offset += length;
    }
  }
  if (!mime) throw new Error("Inline display supports verified PNG and JPEG only; other formats remain available as original MIME parts.");
  if (!width || !height || width > 8192 || height > 8192 || width * height > imagePixelLimit) throw new Error("Inline raster exceeds 8 million pixels or 8192 pixels per side.");
  return { width,height,mime };
}
export async function prepareEmailHTML(session: string, metadata: EmailMetadata, alternative: EmailAlternative, signal: AbortSignal): Promise<{ html:string; warnings:string[] }> {
  const parts = metadata.evidence.inventory!.parts; const body = parts.find((p) => p.path === alternative.part_path)!;
  if (!alternative.display || alternative.display_state !== "available") throw new Error(`Body display is ${alternative.display_state}. Original remains available.`);
  const bytes = await readEmailPart(session,metadata,body,alternative.display,signal);
  const result = sanitizeEmail(new TextDecoder("utf-8",{ fatal:true }).decode(bytes)); signal.throwIfAborted();
  const output = document.createElement("template"); output.innerHTML = result.html;
  let decodedBytes = 0; let pixels = 0; let serialized = new TextEncoder().encode(result.html).byteLength;
  // Sequential fetch + decode bounds live allocation and makes cancellation
  // explicit between every image, hash, decoder and publication boundary.
  for (const image of result.images) {
    signal.throwIfAborted();
    const placeholder = output.content.querySelector(`[data-email-image="${image.index}"]`)!;
    try {
      const part = resolveCID(parts,body.path,image.cid);
      if (!part || part.decode_state !== "decoded" || !part.payload) throw new Error("Missing or ambiguous CID in the nearest related MIME group.");
      if (part.payload.size > imageByteLimit) throw new Error("Inline image exceeds the 5 MiB per-image byte limit.");
      if (decodedBytes + part.payload.size > 16 * MiB) throw new Error("Inline images exceed the 16 MiB aggregate byte limit.");
      decodedBytes += part.payload.size;
      const bytes = await readEmailPart(session,metadata,part,part.payload,signal,imageByteLimit);
      const dimensions = rasterSize(bytes); pixels += dimensions.width * dimensions.height;
      if (pixels > aggregatePixelLimit) throw new Error("Inline images exceed the 32 million aggregate pixel limit.");
      if (part.media.detected && part.media.detected !== dimensions.mime || part.media.declared && part.media.declared !== dimensions.mime) throw new Error("Inline image media disagrees with its verified raster bytes.");
      signal.throwIfAborted();
      const bitmap = await createImageBitmap(new Blob([bytes],{ type:dimensions.mime }));
      try {
        signal.throwIfAborted();
        if (bitmap.width !== dimensions.width || bitmap.height !== dimensions.height) throw new Error("Decoded inline raster dimensions disagree.");
      } finally { bitmap.close(); }
      signal.throwIfAborted();
      let binary = ""; for (let offset=0; offset<bytes.length; offset+=8192) binary += String.fromCharCode(...bytes.subarray(offset,offset+8192));
      const data = `data:${dimensions.mime};base64,${btoa(binary)}`; serialized += data.length + 1024;
      if (serialized > serializedLimit) throw new Error("Inline publication exceeds the 32 MiB serialized display limit.");
      const img = document.createElement("img"); img.src = data; img.alt = image.alt; img.width = dimensions.width; img.height = dimensions.height;
      placeholder.replaceWith(img);
    } catch (cause) {
      signal.throwIfAborted();
      const reason = cause instanceof Error ? cause.message : String(cause);
      placeholder.textContent = `[${image.alt}: ${reason}]`; result.warnings.push(reason);
    }
  }
  signal.throwIfAborted(); const html = output.innerHTML;
  if (new TextEncoder().encode(html).byteLength > serializedLimit) throw new Error("Serialized email exceeds 32 MiB; original remains complete.");
  return { html,warnings:[...new Set(result.warnings)] };
}
export function validFrameEscape(event: MessageEvent, frame: Window | null | undefined, nonce: string): boolean {
  const data = event.data;
  return !!frame && event.source === frame && event.origin === "null" && !!data && typeof data === "object" && !Array.isArray(data) && Object.keys(data).sort().join(",") === "action,channel,nonce" && data.channel === "docbank-email" && data.nonce === nonce && data.action === "escape";
}
