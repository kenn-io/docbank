import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { readRedactionImage, readRedactionText } from "./redactionPreview.js";
import type { RedactionPreview } from "./generated/docbank.js";
const png = Uint8Array.from(
  Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aH2cAAAAASUVORK5CYII=",
    "base64",
  ),
);
const digest = (bytes: Uint8Array) => ({
  size: bytes.length,
  sha256: createHash("sha256").update(bytes).digest("hex"),
});
const text = new TextEncoder().encode("[REDACTED] café\f");
function preview(): RedactionPreview {
  // This reader consumes an already-validated manifest. Use a one-page PNG to
  // isolate transport checks before handing any bytes to the browser decoder.
  return {
    id: "a".repeat(48),
    pages: [
      {
        number: 1,
        pixel_width: 1,
        pixel_height: 1,
        png: digest(png),
        text_start: 0,
        text_end: text.length - 1,
      },
    ],
    archive: { receipt: { text: digest(text) } },
  } as RedactionPreview;
}
function response(bytes: Uint8Array, expected: ReturnType<typeof digest>, media: string) {
  return new Response(Uint8Array.from(bytes), {
    headers: {
      "Content-Length": String(bytes.length),
      "Content-Type": media,
      "X-Content-SHA256": expected.sha256,
    },
  });
}
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
it("rejects corrupted PNG bytes and a hash-consistent wrong dimension before image decode", async () => {
  const decode = vi.fn();
  vi.stubGlobal("createImageBitmap", decode);
  const p = preview(),
    bad = png.slice();
  bad[bad.length - 1] ^= 1;
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValueOnce(response(bad, p.pages[0]!.png, "image/png"));
  await expect(readRedactionImage("test", p, 1, new AbortController().signal)).rejects.toThrow();
  const dimensions = png.slice();
  new DataView(dimensions.buffer).setUint32(16, 2);
  p.pages[0]!.png = digest(dimensions);
  fetch.mockResolvedValueOnce(response(dimensions, p.pages[0]!.png, "image/png"));
  await expect(readRedactionImage("test", p, 1, new AbortController().signal)).rejects.toThrow();
  expect(decode).not.toHaveBeenCalled();
  p.pages[0]!.png.size = 128 * 1024 * 1024 + 1;
  await expect(readRedactionImage("test", p, 1, new AbortController().signal)).rejects.toThrow();
  expect(fetch).toHaveBeenCalledTimes(2);
});
it("checks the complete sidecar before returning a page's UTF-8 byte range", async () => {
  const p = preview();
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValueOnce(response(text, digest(text), "text/plain; charset=utf-8"));
  await expect(readRedactionText("test", p, 1, new AbortController().signal)).resolves.toBe(
    "[REDACTED] café",
  );
  const changed = text.slice();
  changed[0] = 83;
  fetch.mockResolvedValueOnce(response(changed, digest(text), "text/plain; charset=utf-8"));
  await expect(readRedactionText("test", p, 1, new AbortController().signal)).rejects.toThrow();
});
