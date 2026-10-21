import { afterEach, expect, it, vi } from "vite-plus/test";
import { reportAction } from "./action-events.js";

afterEach(() => vi.unstubAllGlobals());

it("posts every action through the daemon with only bounded properties", async () => {
  const fetcher = vi.fn().mockResolvedValue(new Response(null, { status: 202 }));
  vi.stubGlobal("fetch", fetcher);
  reportAction("session", "document_added");
  reportAction("session", "document_opened");
  reportAction("session", "search_run", "lexical");
  reportAction("session", "search_run", "hybrid");
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4));
  expect(fetcher.mock.calls.map(([url, init]) => [url, JSON.parse(init.body)])).toEqual([
    ["/api/daemon/telemetry/events", { event: "document_added", properties: { surface: "web" } }],
    ["/api/daemon/telemetry/events", { event: "document_opened", properties: { surface: "web" } }],
    [
      "/api/daemon/telemetry/events",
      { event: "search_run", properties: { surface: "web", search_type: "text" } },
    ],
    [
      "/api/daemon/telemetry/events",
      { event: "search_run", properties: { surface: "web", search_type: "hybrid" } },
    ],
  ]);
  expect(fetcher.mock.calls[0][1].headers.get("X-Docbank-Web-Session")).toBe("session");
});

it("ignores delivery errors", async () => {
  const unhandled = vi.fn();
  process.on("unhandledRejection", unhandled);
  try {
    const fetcher = vi.fn().mockRejectedValue(new Error("offline"));
    vi.stubGlobal("fetch", fetcher);
    reportAction("session", "document_opened");
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(unhandled).not.toHaveBeenCalled();
  } finally {
    process.off("unhandledRejection", unhandled);
  }
});
