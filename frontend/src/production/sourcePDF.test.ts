import { afterEach, expect, it, vi } from "vitest";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { loadProductionSourcePDF } from "./sourcePDF.js";
import type { ProductionMember } from "./api.js";

const setID = "11111111-1111-4111-8111-111111111111";
const memberID = "22222222-2222-4222-8222-222222222222";
const pdf = new TextEncoder().encode("%PDF-1.7\nsynthetic original page\n%%EOF");
const pdfSHA = bytesToHex(sha256(pdf));
const member: ProductionMember = {
  id: memberID, ordinal: 1, node_id: 7,
  source_version_id: "33333333-3333-4333-8333-333333333333",
  pdf_sha256: pdfSHA, pdf_size: pdf.length, map_sha256: "a".repeat(64),
  mode: "redact_selected", reviewed: false,
};

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function pdfResponse(bytes: Uint8Array = pdf, hash = pdfSHA): Response {
  return new Response(bytes as BodyInit, { status: 200, headers: {
    "Content-Type": "application/pdf",
    "X-Docbank-Blob-Hash": hash,
    "X-Docbank-Blob-Size": String(bytes.length),
    "Content-Digest": `sha-256=:${Buffer.from(sha256(bytes)).toString("base64")}:`,
  } });
}

function draftResponse(etag = 4): Response {
  return new Response(JSON.stringify({ set_id: setID, revision: 2, etag, state: "draft", membership_sealed: false }), {
    status: 200, headers: { "Content-Type": "application/json" },
  });
}

it("loads only a verified exact member PDF and rechecks the draft", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(pdfResponse()).mockResolvedValueOnce(draftResponse());
  const result = await loadProductionSourcePDF("synthetic-session", setID, 2, 4, member, new AbortController().signal);
  expect(Array.from(result)).toEqual(Array.from(pdf));
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(fetch.mock.calls[0]?.[0]).toBe(`/api/v1/productions/sets/${setID}/revisions/2/members/${memberID}/pdf`);
  expect(new Headers((fetch.mock.calls[0]?.[1] as RequestInit).headers).get("If-Match")).toBe("4");
});

it("rejects a mismatched PDF and discards bytes when the draft changes", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(pdfResponse(pdf, "a".repeat(64)));
  await expect(loadProductionSourcePDF("synthetic-session", setID, 2, 4, member,
    new AbortController().signal)).rejects.toThrow();
  vi.restoreAllMocks();
  vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(pdfResponse()).mockResolvedValueOnce(draftResponse(5));
  await expect(loadProductionSourcePDF("synthetic-session", setID, 2, 4, member,
    new AbortController().signal)).rejects.toThrow(/changed/i);
});

it("rejects missing or oversized declared PDF authority before fetching", async () => {
  const fetch = vi.spyOn(globalThis, "fetch");
  await expect(loadProductionSourcePDF("synthetic-session", setID, 2, 4,
    { ...member, pdf_sha256: "", pdf_size: pdf.length }, new AbortController().signal)).rejects.toThrow();
  await expect(loadProductionSourcePDF("synthetic-session", setID, 2, 4,
    { ...member, pdf_size: 64 * 1024 * 1024 + 1 }, new AbortController().signal)).rejects.toThrow();
  expect(fetch).not.toHaveBeenCalled();
});
