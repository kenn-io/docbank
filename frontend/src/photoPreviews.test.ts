// @vitest-environment node
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import type { PhotoPreviewSlot } from "./generated/docbank";
import { PhotoPreviewCache } from "./photoPreviews";

const bytes = new TextEncoder().encode("synthetic JPEG bytes");
const hash = bytesToHex(sha256(bytes));
const asset = "00000000-0000-4000-8000-000000000001";
let created = 0;
let revoked: string[];

beforeEach(() => {
  created = 0;
  revoked = [];
  vi.stubGlobal("location", { origin: "http://localhost" });
  vi.spyOn(URL, "createObjectURL").mockImplementation(() => `blob:synthetic-${++created}`);
  vi.spyOn(URL, "revokeObjectURL").mockImplementation((url) => { revoked.push(url); });
});
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function preview(index = 0): PhotoPreviewSlot {
  const generation = index.toString(16).padStart(64, "0");
  return { state: "ready", url: `/api/v1/photos/assets/${asset}/previews/${generation}`, generation_id: generation, size: bytes.length, sha256: hash, media_type: "image/jpeg", width: 1, height: 1 };
}

function response(p: PhotoPreviewSlot): Response {
  return new Response(bytes, { headers: { "Content-Type": "image/jpeg", "Content-Length": String(bytes.length), ETag: `"${p.generation_id}"`, "Content-Digest": `sha-256=:${Buffer.from(hash, "hex").toString("base64")}:` } });
}

function signal(): AbortSignal { return new AbortController().signal; }

it("deduplicates reads, adds only a header credential and preserves separate leases", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(response(preview()));
  const cache = new PhotoPreviewCache("synthetic-session");
  const [one, two] = await Promise.all([cache.acquire(preview(), signal()), cache.acquire(preview(), signal())]);
  expect(fetch).toHaveBeenCalledOnce();
  expect(one.url).toBe(two.url);
  const [url, init] = fetch.mock.calls[0];
  expect(String(url)).not.toContain("synthetic-session");
  expect(new Headers(init?.headers).get("X-Docbank-Web-Session")).toBe("synthetic-session");
  one.release(); one.release();
  expect(revoked).toEqual([]);
  two.release();
  const again = await cache.acquire(preview(), signal());
  expect(again.url).toBe(one.url);
  again.release(); cache.dispose(); cache.dispose();
  expect(revoked).toEqual([one.url]);
});

it("caps active reads at four and settles queued reads on disposal", async () => {
  const pending: { resolve(response: Response): void; p: PhotoPreviewSlot }[] = [];
  let active = 0;
  let peak = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation((url) => {
    active++; peak = Math.max(active, peak);
    const index = Number.parseInt(String(url).split("/").at(-1)!, 16);
    return new Promise<Response>((resolve) => pending.push({ p: preview(index), resolve: (r) => { active--; resolve(r); } }));
  });
  const cache = new PhotoPreviewCache("session");
  const reads = Array.from({ length: 9 }, (_, i) => cache.acquire(preview(i), signal()));
  const settled = Promise.allSettled(reads);
  expect(pending).toHaveLength(4);
  pending.splice(0).forEach(({ resolve, p }) => resolve(response(p)));
  await vi.waitFor(() => expect(pending).toHaveLength(4));
  cache.dispose();
  pending.splice(0).forEach(({ resolve, p }) => resolve(response(p)));
  const results = await settled;
  expect(peak).toBe(4);
  expect(results.filter((r) => r.status === "rejected")).toHaveLength(5);
  expect(revoked).toHaveLength(4);
  expect(new Set(revoked).size).toBe(4);
});

it("keeps an active lease while evicting idle URLs at 128", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url) => response(preview(Number.parseInt(String(url).split("/").at(-1)!, 16))));
  const cache = new PhotoPreviewCache("session");
  const visible = await cache.acquire(preview(0), signal());
  for (let index = 1; index <= 130; index++) { const lease = await cache.acquire(preview(index), signal()); lease.release(); }
  expect(revoked).toHaveLength(2);
  expect(revoked).not.toContain(visible.url);
  visible.release();
  expect(revoked).toHaveLength(3);
  expect(revoked).not.toContain(visible.url);
  cache.dispose();
  expect(revoked).toHaveLength(131);
  expect(new Set(revoked).size).toBe(131);
});

it("aborts one subscriber without cancelling the other", async () => {
  let finish!: (response: Response) => void;
  let fetchSignal: AbortSignal | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation((_url, init) => { fetchSignal = init?.signal as AbortSignal; return new Promise((resolve) => { finish = resolve; }); });
  const cache = new PhotoPreviewCache("session");
  const controller = new AbortController();
  const cancelled = cache.acquire(preview(), controller.signal);
  const check = expect(cancelled).rejects.toBeDefined();
  const retained = cache.acquire(preview(), signal());
  controller.abort();
  expect(fetchSignal?.aborted).toBe(false);
  finish(response(preview()));
  await check;
  const lease = await retained; lease.release(); cache.dispose();
  expect(revoked).toHaveLength(1);
});

it.each(["abort", "dispose"])("discards delayed completion after %s even if fetch ignores abort", async (action) => {
  let finish!: (response: Response) => void;
  vi.spyOn(globalThis, "fetch").mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
  const cache = new PhotoPreviewCache("session");
  const controller = new AbortController();
  const read = cache.acquire(preview(), controller.signal);
  const check = expect(read).rejects.toBeDefined();
  if (action === "abort") controller.abort(); else cache.dispose();
  finish(response(preview()));
  await check;
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(created).toBe(0); expect(revoked).toEqual([]);
  cache.dispose();
  await expect(cache.acquire(preview(), signal())).rejects.toThrow("disposed");
});

it("rejects response disagreement and permits an explicit retry", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(new Response(bytes, { headers: { "Content-Type": "image/png" } })).mockResolvedValueOnce(response(preview()));
  const cache = new PhotoPreviewCache("session");
  await expect(cache.acquire(preview(), signal())).rejects.toThrow("disagrees");
  const lease = await cache.acquire(preview(), signal());
  expect(fetch).toHaveBeenCalledTimes(2); lease.release(); cache.dispose();
});

it("rejects unsupported URLs and conflicting generation metadata before another read", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(response(preview()));
  const cache = new PhotoPreviewCache("session");
  for (const url of ["https://example.com/preview", `${preview().url}?token=x`, `${preview().url}#fragment`, "/api/v1/content/bytes"]) {
    await expect(cache.acquire({ ...preview(), url }, signal())).rejects.toThrow("outside");
  }
  await expect(cache.acquire({ state: "missing" }, signal())).rejects.toThrow("metadata");
  expect(fetch).not.toHaveBeenCalled();
  const lease = await cache.acquire(preview(), signal());
  await expect(cache.acquire({ ...preview(), size: bytes.length + 1 }, signal())).rejects.toThrow("metadata changed");
  lease.release(); cache.dispose();
});

it.each(["digest", "truncated", "identity"])("refuses %s disagreement before creating a blob URL", async (kind) => {
  const result = response(preview());
  const headers = new Headers(result.headers);
  let body: Uint8Array<ArrayBuffer> = bytes;
  if (kind === "digest") body = new Uint8Array(bytes.length);
  if (kind === "truncated") body = bytes.slice(1);
  if (kind === "identity") headers.set("ETag", `"${"f".repeat(64)}"`);
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, { headers }));
  const cache = new PhotoPreviewCache("session");
  await expect(cache.acquire(preview(), signal())).rejects.toThrow();
  expect(created).toBe(0); expect(revoked).toEqual([]); cache.dispose();
});
