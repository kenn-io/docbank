import type { GetExportJobEvents200 } from "./generated/docbank.js";
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

it.each(["end", "cancel"])(
  "reads export progress incrementally before stream %s",
  async (finish) => {
    const running: GetExportJobEvents200 = {
      delivery: "current_state",
      requested_after: 0,
      job: {
        id: "synthetic-job",
        plan_id: "synthetic-plan",
        fingerprint: "a".repeat(64),
        attempt: 1,
        sequence: 1,
        state: "running",
        completed_bytes: 0,
        completed_roles: 0,
        created_at: "2026-09-01T00:00:00Z",
        deadline: "2026-09-01T02:00:00Z",
        expires_at: "2026-09-01T02:00:00Z",
      },
    };
    const failed = {
      ...running,
      job: { ...running.job, sequence: 2, state: "failed", failure: "archive_failed" },
    };
    const lastLine = JSON.stringify(failed) + "\n";
    const encoder = new TextEncoder();
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    const cancel = vi.fn();
    const body = new ReadableStream<Uint8Array>({
      start(value) {
        controller = value;
      },
      cancel,
    });
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(body, { headers: { "Content-Type": "application/x-ndjson" } }),
    );
    const events = streamExportJobEvents("synthetic-job");
    try {
      controller.enqueue(encoder.encode(JSON.stringify(running) + "\n" + lastLine.slice(0, 20)));
      expect(await events.next()).toEqual({ done: false, value: running });
      if (finish === "cancel") {
        await events.return(undefined);
        expect(cancel).toHaveBeenCalledOnce();
      } else {
        controller.enqueue(encoder.encode(lastLine.slice(20)));
        controller.close();
        expect(await events.next()).toEqual({ done: false, value: failed });
        expect((await events.next()).done).toBe(true);
      }
      expect(body.locked).toBe(false);
    } finally {
      await events.return(undefined);
    }
  },
);
