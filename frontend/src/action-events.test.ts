import { afterEach, expect, it, vi } from "vite-plus/test";
import { reportAction, reportFailure, reportSearch } from "./action-events.js";
import { APIError } from "./api-transport.js";

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

it("reports an empty search after its search run", async () => {
  const fetcher = vi.fn().mockResolvedValue(new Response(null, { status: 202 }));
  vi.stubGlobal("fetch", fetcher);
  reportSearch("session", "lexical", true);
  reportSearch("session", "semantic", false);
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3));
  expect(fetcher.mock.calls.map(([, init]) => JSON.parse(init.body))).toEqual([
    { event: "search_run", properties: { surface: "web", search_type: "text" } },
    { event: "search_empty", properties: { surface: "web", search_type: "text" } },
    { event: "search_run", properties: { surface: "web", search_type: "semantic" } },
  ]);
});

it.each<[string, unknown, number]>([
  ["a failed request", new Error("Upload failed"), 1],
  ["a server failure", new APIError("Unavailable", 500, "internal"), 1],
  ["a sign-in prompt", new APIError("Unauthorized", 401, "unauthorized"), 0],
  ["a cancel", new DOMException("aborted", "AbortError"), 0],
])("reports %s as an error with its kind", async (_name, cause, sent) => {
  const fetcher = vi.fn().mockResolvedValue(new Response(null, { status: 202 }));
  vi.stubGlobal("fetch", fetcher);
  reportFailure("session", "add", cause);
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(fetcher.mock.calls.map(([, init]) => JSON.parse(init.body))).toEqual(
    sent ? [{ event: "error_shown", properties: { surface: "web", kind: "add" } }] : [],
  );
});
