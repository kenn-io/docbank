import { afterEach, expect, it, vi } from "vitest";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { loadSelectableTextPage, textSpanForSelection } from "./textSelection.js";

const setID = "11111111-1111-4111-8111-111111111111";
const memberID = "22222222-2222-4222-8222-222222222222";
const frame = { page: 2, sha256: "c".repeat(64), width: 10000, height: 10000 };
const raw = new TextEncoder().encode(JSON.stringify({ contract: "aligned-text/v1", text: "First\nA😀B",
  pages: [{ number: 1, frame_sha256: "d".repeat(64), width: 10000, height: 10000, span: { start: 0, end: 5 } },
    { number: 2, frame_sha256: frame.sha256, width: frame.width, height: frame.height, span: { start: 6, end: 12 } }] }));
const digest = bytesToHex(sha256(raw));
const draft = { set_id: setID, revision: 2, etag: 4, state: "draft" };

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function responses(mapSHA = digest, etag = 4): void {
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    if (url.endsWith(`/maps/${memberID}?limit=65536`)) return new Response(JSON.stringify({ map_sha256: mapSHA,
      offset: 0, total_bytes: raw.length, data: Buffer.from(raw).toString("base64"),
      chunk_sha256: bytesToHex(sha256(raw)), next_cursor: "" }), { headers: { "Content-Type": "application/json" } });
    if (url.endsWith(`/revisions/2`)) return new Response(JSON.stringify({ ...draft, etag }),
      { headers: { "Content-Type": "application/json" } });
    throw new Error(`unexpected request ${url}`);
  });
}

it("loads a digest-verified page and converts UTF-16 selection into global UTF-8 bytes", async () => {
  responses();
  const page = await loadSelectableTextPage("synthetic", setID, 2, 4, memberID, digest, frame, new AbortController().signal);
  expect(page).toEqual({ text: "A😀B", start: 6, frame });
  expect(textSpanForSelection(page, 1, 3)).toEqual({ start: 7, end: 11 });
  expect(() => textSpanForSelection(page, 1, 2)).toThrow(/surrogate/);
  expect(() => textSpanForSelection(page, 2, 2)).toThrow();
});

it("rejects changed map or draft authority", async () => {
  responses("b".repeat(64));
  await expect(loadSelectableTextPage("synthetic", setID, 2, 4, memberID, digest, frame, new AbortController().signal)).rejects.toThrow();
  vi.restoreAllMocks();
  responses(digest, 5);
  await expect(loadSelectableTextPage("synthetic", setID, 2, 4, memberID, digest, frame, new AbortController().signal)).rejects.toThrow();
});

it("rejects page-frame drift even if the map digest is valid", async () => {
  responses();
  await expect(loadSelectableTextPage("synthetic", setID, 2, 4, memberID, digest,
    { ...frame, sha256: "e".repeat(64) }, new AbortController().signal)).rejects.toThrow(/frame/);
});
