import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { inspectionReporter, reportAction } from "./action-events.js";

let day = 0;
beforeEach(() => vi.setSystemTime(new Date(Date.UTC(2030, 0, ++day))));
afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

it("shares successful daily inspection delivery across viewers and retries failures", async () => {
  let resolve: (response: Response) => void = () => undefined;
  const pending = new Promise<Response>((done) => {
    resolve = done;
  });
  const fetcher = vi
    .spyOn(globalThis, "fetch")
    .mockReturnValueOnce(pending)
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValue(new Response(null, { status: 202 }));
  const live = inspectionReporter("document_opened", () => "live-session");
  const snapshot = inspectionReporter("document_opened", () => "snapshot-session");
  live();
  snapshot();
  expect(fetcher).toHaveBeenCalledTimes(1);
  resolve(new Response(null, { status: 503 }));
  await new Promise((done) => setTimeout(done, 0));
  snapshot();
  await new Promise((done) => setTimeout(done, 0));
  live();
  await new Promise((done) => setTimeout(done, 0));
  snapshot();
  expect(fetcher).toHaveBeenCalledTimes(3);
  vi.setSystemTime(new Date(Date.UTC(2030, 0, day + 1)));
  snapshot();
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4));
  reportAction("live-session", "search_run", "hybrid");
  reportAction("live-session", "search_run", "hybrid");
  expect(fetcher).toHaveBeenCalledTimes(6);
});
