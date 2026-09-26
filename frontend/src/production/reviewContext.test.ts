import { afterEach, expect, it, vi } from "vitest";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { loadReviewContext } from "./reviewContext.js";

const setID = "11111111-1111-4111-8111-111111111111";
const memberID = "22222222-2222-4222-8222-222222222222";
const encoded = new TextEncoder().encode(JSON.stringify({ contract: "aligned-text/v1", text: "A😀B secret", pages: [] }));
const digest = bytesToHex(sha256(encoded));
const selector = { kind: "text", map_sha256: digest, span: { start: 1, end: 5 } };

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function chunk(data: Uint8Array, mapSHA = digest): Response {
  return new Response(JSON.stringify({ map_sha256: mapSHA, offset: 0, total_bytes: data.length,
    data: Buffer.from(data).toString("base64"), chunk_sha256: bytesToHex(sha256(data)), next_cursor: "" }),
  { status: 200, headers: { "Content-Type": "application/json" } });
}

it("shows exact UTF-8 selected text only after verifying the retained map", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(chunk(encoded));
  const context = await loadReviewContext("synthetic", setID, 2, memberID, selector, new AbortController().signal);
  expect(context.selected).toBe("😀");
  expect(context.before).toBe("A");
  expect(context.after).toBe("B secret");
});

it("rejects a changed map digest and unsupported visual-only selectors", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(chunk(encoded, "b".repeat(64)));
  await expect(loadReviewContext("synthetic", setID, 2, memberID, selector, new AbortController().signal)).rejects.toThrow();
  await expect(loadReviewContext("synthetic", setID, 2, memberID,
    { kind: "rectangle", map_sha256: digest }, new AbortController().signal)).rejects.toThrow();
});
