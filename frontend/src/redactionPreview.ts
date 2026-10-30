import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { sha256Digest } from "./crypto.js";
import { sessionJSON, sessionResponse } from "./api-transport.js";
import { readExactBody } from "./download.js";
import { pageDigest } from "./pages.js";
import {
  redactionRoot,
  redactionObject as object,
  redactionInteger as integer,
  redactionHash as hash,
  requireRedaction as check,
} from "./redaction.js";
import type {
  RedactionPreview,
  RedactionReview,
  RedactionPreparation,
  RedactionMask,
  RedactionFileDigest,
} from "./generated/docbank.js";

const pngLimit = 128 * 1024 * 1024;
const same = (a: unknown, b: unknown) => pageDigest(a) === pageDigest(b);
function digest(v: unknown, max: number) {
  const o = object(v, ["size", "sha256"]);
  integer(o.size, 0, max);
  hash(o.sha256);
}
export function decodeRedactionPreview(value: unknown, review: RedactionReview): RedactionPreview {
  const o = object(value, ["contract", "id", "expires_at", "archive", "pages"], ["$schema"]);
  check(
    o.contract === "native-pdf-redaction-preview/v1" &&
      typeof o.id === "string" &&
      /^[0-9a-f]{48}$/.test(o.id),
  );
  check(typeof o.expires_at === "string" && Number.isFinite(Date.parse(o.expires_at)));
  const a = object(o.archive, ["receipt", "size", "sha256"]);
  integer(a.size, 1, 544 * 1024 * 1024);
  hash(a.sha256);
  const r = object(a.receipt, [
    "contract",
    "selection",
    "admitted_node_revision",
    "preparation_sha256",
    "decisions_sha256",
    "review_sha256",
    "resolved_sha256",
    "recipe",
    "page_count",
    "pdf",
    "text",
  ]);
  check(
    r.contract === "native-pdf-redaction-result/v1" &&
      same(r.selection, review.selection) &&
      r.review_sha256 === review.sha256 &&
      r.preparation_sha256 === review.preparation_sha256 &&
      r.decisions_sha256 === review.decisions_sha256 &&
      r.resolved_sha256 === review.resolved.sha256 &&
      same(r.recipe, review.recipe) &&
      r.page_count === review.resolved.page_count,
  );
  integer(r.admitted_node_revision, 1);
  digest(r.pdf, 512 * 1024 * 1024);
  digest(r.text, 16 * 1024 * 1024 + 16);
  check(
    Array.isArray(o.pages) && o.pages.length === review.resolved.page_count && o.pages.length <= 16,
  );
  let start = 0;
  let sanitized = "";
  for (const [i, p] of o.pages.entries()) {
    const page = object(p, [
      "number",
      "frame_sha256",
      "width",
      "height",
      "pixel_width",
      "pixel_height",
      "png",
      "text_start",
      "text_end",
    ]);
    const expected = review.resolved.pages[i]!;
    check(
      page.number === i + 1 &&
        page.frame_sha256 === expected.frame_sha256 &&
        page.width === expected.width &&
        page.height === expected.height,
    );
    const w = integer(page.pixel_width, 1, 16384),
      h = integer(page.pixel_height, 1, 16384);
    check(
      w * h <= 40000000 &&
        w === Math.ceil((expected.width * review.recipe.dpi) / 10000) &&
        h === Math.ceil((expected.height * review.recipe.dpi) / 10000),
    );
    digest(page.png, pngLimit);
    integer((page.png as RedactionFileDigest).size, 33, pngLimit);
    const text = review.resolved.runs
      .filter((run) => run.page === i + 1)
      .map((run) => run.text)
      .join("");
    sanitized += text + "\f";
    const end = start + new TextEncoder().encode(text).length;
    check(page.text_start === start && page.text_end === end);
    start = end + 1;
  }
  check(
    (r.text as RedactionFileDigest).size === start &&
      (r.text as RedactionFileDigest).sha256 ===
        bytesToHex(sha256(new TextEncoder().encode(sanitized))),
  );
  return o as unknown as RedactionPreview;
}
export async function createRedactionPreview(
  session: string,
  preparation: RedactionPreparation,
  masks: RedactionMask[],
  review: RedactionReview,
  signal: AbortSignal,
) {
  return decodeRedactionPreview(
    await sessionJSON<unknown>(`${redactionRoot}/previews`, {
      session,
      signal,
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ preparation, masks, review_sha256: review.sha256 }),
      responseLimit: 256 << 10,
    }),
    review,
  );
}
async function previewBytes(
  session: string,
  preview: RedactionPreview,
  part: string,
  expected: RedactionFileDigest,
  media: string,
  signal: AbortSignal,
) {
  const response = await sessionResponse(`${redactionRoot}/previews/${preview.id}/${part}`, {
    session,
    signal,
  });
  try {
    check(
      response.headers.get("Content-Length") === String(expected.size) &&
        response.headers.get("X-Content-SHA256") === expected.sha256 &&
        response.headers.get("Content-Type") === media,
    );
  } catch (cause) {
    await response.body?.cancel();
    throw cause;
  }
  const bytes = await readExactBody(response, expected.size, signal);
  check(bytesToHex(await sha256Digest(bytes)) === expected.sha256);
  signal.throwIfAborted();
  return bytes;
}
export async function readRedactionImage(
  session: string,
  preview: RedactionPreview,
  pageNumber: number,
  signal: AbortSignal,
) {
  const page = preview.pages[integer(pageNumber, 1, preview.pages.length) - 1]!;
  digest(page.png, pngLimit);
  const bytes = await previewBytes(
    session,
    preview,
    `pages/${pageNumber}`,
    page.png,
    "image/png",
    signal,
  );
  check(
    bytes.length >= 33 &&
      [137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82].every(
        (v, i) => bytes[i] === v,
      ),
  );
  const view = new DataView(bytes.buffer);
  check(view.getUint32(16) === page.pixel_width && view.getUint32(20) === page.pixel_height);
  const blob = new Blob([bytes], { type: "image/png" });
  const image = await createImageBitmap(blob);
  try {
    signal.throwIfAborted();
    check(image.width === page.pixel_width && image.height === page.pixel_height);
  } finally {
    image.close();
  }
  return URL.createObjectURL(blob);
}
export async function readRedactionText(
  session: string,
  preview: RedactionPreview,
  pageNumber: number,
  signal: AbortSignal,
) {
  const page = preview.pages[integer(pageNumber, 1, preview.pages.length) - 1]!;
  digest(preview.archive.receipt.text, 16 * 1024 * 1024 + 16);
  const bytes = await previewBytes(
    session,
    preview,
    "text",
    preview.archive.receipt.text,
    "text/plain; charset=utf-8",
    signal,
  );
  const decoder = new TextDecoder("utf-8", { fatal: true });
  decoder.decode(bytes);
  return decoder.decode(bytes.subarray(page.text_start, page.text_end));
}
export async function redactionTicket(
  session: string,
  preview: RedactionPreview,
  signal: AbortSignal,
): Promise<string> {
  const result = object(
    await sessionJSON(`${redactionRoot}/previews/${preview.id}/download`, {
      session,
      signal,
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        confirmed: true,
        review_sha256: preview.archive.receipt.review_sha256,
        archive_sha256: preview.archive.sha256,
      }),
      responseLimit: 4096,
    }),
    ["url", "size", "sha256"],
    ["$schema"],
  );
  check(
    result.size === preview.archive.size &&
      result.sha256 === preview.archive.sha256 &&
      typeof result.url === "string" &&
      /^\/api\/daemon\/web-download\/file\?ticket=[A-Za-z0-9_-]+$/.test(result.url),
  );
  return result.url;
}
