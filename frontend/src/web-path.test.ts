import { afterEach, expect, it, vi } from "vite-plus/test";
import { webPath, webURL } from "./web-path.js";
import { sessionResponse } from "./api-transport.js";

afterEach(() => {
  document.querySelector('meta[name="docbank-base-path"]')?.remove();
  vi.restoreAllMocks();
});

it("keeps root deployments unchanged", () => {
  expect(webURL("/api/v1/tags")).toBe("/api/v1/tags");
  expect(webPath("/photos/albums")).toBe("/photos/albums");
});

it("uses the server mount for requests and navigation, including deep links", async () => {
  const meta = document.createElement("meta");
  meta.name = "docbank-base-path";
  meta.content = "/tools/archive";
  document.head.append(meta);
  expect(webPath("/tools/archive/photos/albums")).toBe("/photos/albums");
  expect(webPath("/tools/archive-other/photos")).toBe("");
  expect(webURL("/photos#photo=abc")).toBe("/tools/archive/photos#photo=abc");
  expect(webURL("https://example.test/external")).toBe("https://example.test/external");
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({}));
  await sessionResponse("/api/v1/tags", { session: "synthetic-session" });
  expect(fetch.mock.calls[0]?.[0]).toBe("/tools/archive/api/v1/tags");
  expect(new Headers(fetch.mock.calls[0]?.[1]?.headers).get("X-Docbank-Web-Session")).toBe(
    "synthetic-session",
  );
});
