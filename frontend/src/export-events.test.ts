import { afterEach, expect, it, vi } from "vite-plus/test";
import { streamExportJobEvents } from "./export-events.js";

afterEach(() => vi.restoreAllMocks());

it("rejects truncated and oversized progress streams", async () => {
  for (const [content, error] of [
    ["{}", /progress stream was truncated/],
    ["a".repeat(65537) + "\n", /progress event was unexpectedly large/],
  ] as const) {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(content, { headers: { "Content-Type": "application/x-ndjson" } }),
    );
    await expect(
      streamExportJobEvents("33333333-3333-4333-8333-333333333333", undefined, {
        session: "s",
        signal: new AbortController().signal,
      }).next(),
    ).rejects.toThrow(error);
  }
});
