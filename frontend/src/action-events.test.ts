import { afterEach, expect, it, vi } from "vite-plus/test";
import { inspectionReporter, reportAction } from "./action-events.js";

afterEach(() => vi.restoreAllMocks());

it("forwards each inspection intent across viewers, including after a failed report", () => {
  const fetcher = vi
    .spyOn(globalThis, "fetch")
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValue(new Response(null, { status: 202 }));
  const live = inspectionReporter("document_opened", () => "live-session");
  const snapshot = inspectionReporter("document_opened", () => "snapshot-session");
  live();
  snapshot();
  live();
  snapshot();
  expect(fetcher).toHaveBeenCalledTimes(4);
  const opened = { event: "document_opened", properties: { surface: "web" } };
  expect(fetcher.mock.calls.map(([, init]) => JSON.parse(init?.body as string))).toEqual([
    opened,
    opened,
    opened,
    opened,
  ]);
  reportAction("live-session", "search_run", "hybrid");
  reportAction("live-session", "search_run", "hybrid");
  expect(fetcher.mock.calls.slice(4).map(([, init]) => JSON.parse(init?.body as string))).toEqual([
    { event: "search_run", properties: { surface: "web", search_type: "hybrid" } },
    { event: "search_run", properties: { surface: "web", search_type: "hybrid" } },
  ]);
});
