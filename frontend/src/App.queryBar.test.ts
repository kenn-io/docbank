import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import App from "./App.svelte";
import { canonicalQuery, parseQuery } from "./query.js";
import { snapshotEnvelope } from "./snapshot-test-fixtures.js";
import { root } from "./version-test-fixtures.js";

const query = {
  v: 1,
  text: "name:manual OR saved:review",
  syntax: "advanced",
  mode: "lexical",
  filters: { exclude_tag_ids: ["11111111-1111-4111-8111-111111111111"] },
  sort: { field: "size", direction: "desc" },
};
afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function start(withQuery = true) {
  history.replaceState(
    null,
    "",
    `/#web_session=synthetic&web_upload_secret=proof${withQuery ? `&query=${encodeURIComponent(JSON.stringify(query))}` : ""}`,
  );
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/queries/parse")
      return new Response(
        JSON.stringify({
          query: JSON.parse(String(init?.body)),
          query_fingerprint: `sha256:${createHash("sha256").update(String(init?.body)).digest("hex")}`,
          dependencies: [],
        }),
      );
    const value = url.includes("/path?")
      ? root
      : url.includes("/children?")
        ? { directory: root, items: [], total: 0, limit: 1000, offset: 0 }
        : { items: [], total: 0, limit: url.includes("saved-queries") ? 100 : 1000, offset: 0 };
    return new Response(JSON.stringify(value));
  });
  render(App);
  return fetch;
}

it("opens URL intent and passes the entire edited query to Save without executing", async () => {
  const fetch = start();
  await screen.findByRole("dialog", { name: "Filters and sort" });
  expect((screen.getByLabelText("Query syntax") as HTMLTextAreaElement).value).toBe(query.text);
  await fireEvent.input(screen.getByLabelText("Query syntax"), {
    target: { value: "NOT tag:missing" },
  });
  await waitFor(() =>
    expect(JSON.parse(new URLSearchParams(location.hash.slice(1)).get("query")!)).toEqual({
      ...query,
      text: "NOT tag:missing",
    }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Save…" }));
  await screen.findByRole("dialog", { name: "Saved searches" });
  expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull();
  await fireEvent.click(screen.getByText("Edit as JSON"));
  expect(
    JSON.parse((screen.getByLabelText("Complete query JSON") as HTMLTextAreaElement).value),
  ).toEqual({ ...query, text: "NOT tag:missing" });
  await fireEvent.click(screen.getByRole("button", { name: "Close" }));
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  expect((screen.getByLabelText("Query syntax") as HTMLTextAreaElement).value).toBe(
    "NOT tag:missing",
  );
  expect(fetch.mock.calls.some(([url]) => String(url).startsWith("/api/v1/search"))).toBe(false);
});

it("retains unfinished edits on Escape, then discards them with Cancel", async () => {
  start();
  await screen.findByRole("dialog", { name: "Filters and sort" });
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":' },
  });
  await fireEvent.input(screen.getByLabelText("Query syntax"), {
    target: { value: "unfinished draft" },
  });
  await fireEvent.keyDown(window, { key: "Escape" });
  expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull();
  await fireEvent.focusIn(screen.getByRole("button", { name: "Advanced" }));
  expect((await screen.findByRole("tooltip")).textContent).toBe("Unapplied changes");
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe('{"size_min":');
  expect((screen.getByLabelText("Query syntax") as HTMLTextAreaElement).value).toBe(
    "unfinished draft",
  );
  await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(location.hash).toBe("");
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe("{}");
});

it("clears the indicator when reopened edits are restored to their original value", async () => {
  start(false);
  await screen.findByRole("button", { name: "Advanced" });
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":2}' },
  });
  await fireEvent.keyDown(window, { key: "Escape" });
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.input(screen.getByLabelText("Filters JSON"), { target: { value: "{}" } });
  await fireEvent.keyDown(window, { key: "Escape" });
  expect(screen.getByRole("button", { name: "Advanced" }).textContent).not.toContain("•");
  expect(new URLSearchParams(location.hash.slice(1)).has("query")).toBe(false);
});

it("toggles Advanced and restores edited drafts after an outside click", async () => {
  start(false);
  await screen.findByRole("button", { name: "Advanced" });
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":2}' },
  });
  const overlay = screen.getByRole("dialog", { name: "Filters and sort" }).parentElement!;
  await fireEvent.pointerDown(overlay, { pointerId: 1 });
  await fireEvent.pointerUp(overlay, { pointerId: 1 });
  await fireEvent.click(overlay);
  expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe(
    '{"size_min":2}',
  );
});

it("runs unsent top-bar text with the selected filters", async () => {
  const fetch = start(false);
  const search = await screen.findByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(search, { target: { value: "annual report" } });
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":10}' },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() =>
    expect(
      fetch.mock.calls.some(
        ([url, init]) =>
          String(url) === "/api/v1/workspace/queries" &&
          JSON.parse(String(init?.body)).query.text === "annual report",
      ),
    ).toBe(true),
  );
  const body = JSON.parse(
    String(
      fetch.mock.calls.find(([url]) => String(url) === "/api/v1/workspace/queries")?.[1]?.body,
    ),
  );
  expect(body.query.filters).toEqual({ size_min: 10 });
});

it("shows the filter error instead of running the last valid filters from the top bar", async () => {
  const fetch = start(false);
  const search = await screen.findByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(search, { target: { value: "annual report" } });
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":' },
  });
  await fireEvent.keyDown(window, { key: "Escape" });
  expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull();
  await fireEvent.submit(search.closest("form")!);
  await screen.findByRole("dialog", { name: "Filters and sort" });
  expect(screen.getAllByRole("alert").length).toBeGreaterThan(0);
  expect(fetch.mock.calls.some(([url]) => String(url) === "/api/v1/workspace/queries")).toBe(false);
});

it("runs the top bar with the page size chosen in Advanced", async () => {
  const fetch = start(false);
  const search = await screen.findByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(search, { target: { value: "annual report" } });
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.click(screen.getByRole("combobox", { name: "Documents per page: 100" }));
  await fireEvent.click(screen.getByRole("option", { name: "250" }));
  await fireEvent.keyDown(window, { key: "Escape" });
  await fireEvent.submit(search.closest("form")!);
  await waitFor(() =>
    expect(fetch.mock.calls.some(([url]) => String(url) === "/api/v1/workspace/queries")).toBe(
      true,
    ),
  );
  const body = JSON.parse(
    String(
      fetch.mock.calls.find(([url]) => String(url) === "/api/v1/workspace/queries")?.[1]?.body,
    ),
  );
  expect(body.page_size).toBe(250);
});

it("keeps a saved plain-words search's filters when the search text changes", async () => {
  const saved = parseQuery(
    JSON.stringify({ text: "invoice", filters: { extensions: ["pdf"] }, sort: { field: "size" } }),
  );
  const canonical = canonicalQuery(saved);
  const record = {
    id: "11111111-1111-4111-8111-111111111111",
    name: "Invoices",
    description: "",
    kind: "query",
    payload: JSON.parse(canonical),
    revision: 1,
    fingerprint: `sha256:${createHash("sha256").update(canonical).digest("hex")}`,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
  const searches: unknown[] = [];
  history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/workspace/queries") {
      const body = JSON.parse(String(init?.body));
      searches.push(body.query);
      return new Response(
        JSON.stringify(await snapshotEnvelope(parseQuery(JSON.stringify(body.query)), [])),
      );
    }
    if (url.includes("saved-queries"))
      return new Response(JSON.stringify({ items: [record], total: 1, limit: 100, offset: 0 }));
    const value = url.includes("/path?")
      ? root
      : url.includes("/children?")
        ? { directory: root, items: [], total: 0, limit: 1000, offset: 0 }
        : { items: [], total: 0, limit: 1000, offset: 0 };
    return new Response(JSON.stringify(value));
  });
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Saved searches" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Run Invoices" }));
  await waitFor(() => expect(searches).toHaveLength(1));
  const search = await screen.findByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(search, { target: { value: "receipts" } });
  await fireEvent.submit(search.closest("form")!);
  await waitFor(() => expect(searches).toHaveLength(2));
  expect(searches[1]).toMatchObject({ text: "receipts", filters: { extensions: ["pdf"] } });
});

it("uses submitted top-bar text after repairing retained invalid filters", async () => {
  const fetch = start(false);
  const search = await screen.findByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(search, { target: { value: "old text" } });
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":' },
  });
  await fireEvent.keyDown(window, { key: "Escape" });
  await fireEvent.input(search, { target: { value: "new text" } });
  await fireEvent.submit(search.closest("form")!);
  expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe('{"size_min":');
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":10}' },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() =>
    expect(fetch.mock.calls.some(([url]) => String(url) === "/api/v1/workspace/queries")).toBe(
      true,
    ),
  );
  const request = fetch.mock.calls.find(([url]) => String(url) === "/api/v1/workspace/queries")!;
  expect(JSON.parse(String(request[1]?.body)).query).toMatchObject({
    text: "new text",
    filters: { size_min: 10 },
  });
});
