import { afterEach, expect, it, vi } from "vitest";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { loadProductionPreview, StaleProductionPreviewError } from "./preview.js";

const setID = "11111111-1111-4111-8111-111111111111";
const memberID = "22222222-2222-4222-8222-222222222222";
const operationID = "33333333-3333-4333-8333-333333333333";
const image = new TextEncoder().encode("synthetic PNG bytes");
const text = new TextEncoder().encode("Synthetic retained text");
const hash = (bytes: Uint8Array) => bytesToHex(sha256(bytes));
const imageURL = `/api/daemon/web-download/file?ticket=${"a".repeat(43)}`;
const textURL = `/api/daemon/web-download/file?ticket=${"b".repeat(43)}`;
const ticket = { operation_id: operationID, preview_input_sha256: "c".repeat(64),
  resolved_sha256: "d".repeat(64), image: { url: imageURL, sha256: hash(image), size: image.length },
  text: { url: textURL, sha256: hash(text), size: text.length } };

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function artifact(bytes: Uint8Array, mediaType: string): Response {
  return new Response(bytes as BodyInit, { headers: { "Content-Type": mediaType,
    "Content-Length": String(bytes.length), "X-Docbank-Blob-Hash": hash(bytes),
    "X-Docbank-Blob-Size": String(bytes.length),
    "Content-Digest": `sha-256=:${Buffer.from(sha256(bytes)).toString("base64")}:` } });
}

function respond(override: object = ticket, draftETag = 4) {
  return vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith("/previews")) {
      expect(init?.method).toBe("POST");
      expect(new Headers(init?.headers).get("If-Match")).toBe("4");
      expect(JSON.parse(String(init?.body))).toEqual({ operation_id: operationID, member_id: memberID, page: 1 });
      return Response.json(override);
    }
    if (url === imageURL) return artifact(image, "image/png");
    if (url === textURL) return artifact(text, "text/plain; charset=utf-8");
    if (url.endsWith("/revisions/2")) return Response.json({ set_id: setID, revision: 2, etag: draftETag, state: "draft" });
    throw new Error(`unexpected request ${url}`);
  });
}

it("verifies one-use image and text tickets before showing an exact draft preview", async () => {
  const fetch = respond();
  const result = await loadProductionPreview("synthetic", setID, 2, 4, memberID, 1, operationID,
    new AbortController().signal);
  expect(Array.from(result.image)).toEqual(Array.from(image));
  expect(result.text).toBe("Synthetic retained text");
  expect(fetch.mock.calls.map(call => String(call[0]))).toEqual([
    `/api/v1/productions/sets/${setID}/revisions/2/previews`, imageURL, textURL,
    `/api/v1/productions/sets/${setID}/revisions/2`,
  ]);
});

it("rejects an external ticket or a ticket for another operation before downloading", async () => {
  const fetch = respond({ ...ticket, image: { ...ticket.image, url: "https://example.invalid/file" } });
  await expect(loadProductionPreview("synthetic", setID, 2, 4, memberID, 1, operationID,
    new AbortController().signal)).rejects.toThrow(/ticket|url/i);
  expect(fetch).toHaveBeenCalledTimes(1);
  vi.restoreAllMocks();
  respond({ ...ticket, operation_id: "44444444-4444-4444-8444-444444444444" });
  await expect(loadProductionPreview("synthetic", setID, 2, 4, memberID, 1, operationID,
    new AbortController().signal)).rejects.toThrow(/operation/i);
  vi.restoreAllMocks();
  respond({ ...ticket, image: null });
  await expect(loadProductionPreview("synthetic", setID, 2, 4, memberID, 1, operationID,
    new AbortController().signal)).rejects.toThrow(/ticket/i);
});

it("rejects mismatched artifact bytes and a changed draft", async () => {
  respond({ ...ticket, image: { ...ticket.image, sha256: "f".repeat(64) } });
  await expect(loadProductionPreview("synthetic", setID, 2, 4, memberID, 1, operationID,
    new AbortController().signal)).rejects.toThrow(/image|preview/i);
  vi.restoreAllMocks();
  respond(ticket, 5);
  await expect(loadProductionPreview("synthetic", setID, 2, 4, memberID, 1, operationID,
    new AbortController().signal)).rejects.toBeInstanceOf(StaleProductionPreviewError);
});
