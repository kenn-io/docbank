import { createHash } from "node:crypto";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { tick } from "svelte";
import QueryBar from "./QueryBar.svelte";
import { parseQuery } from "./query.js";

const canonical =
  '{"filters":{"exclude_tag_ids":["11111111-1111-4111-8111-111111111111"],"size_min":2},"mode":"lexical","sort":{"direction":"desc","field":"size"},"syntax":"advanced","text":"name:manual OR collection:archive","v":1}';
const query = parseQuery(canonical);
const response = (text = canonical) =>
  new Response(
    JSON.stringify({
      query: JSON.parse(text),
      query_fingerprint: `sha256:${createHash("sha256").update(text).digest("hex")}`,
      dependencies: [],
    }),
  );
beforeEach(() => {
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});
function open() {
  const onchange = vi.fn();
  const onclose = vi.fn();
  const onrun = vi.fn();
  const oncancel = vi.fn();
  const view = render(QueryBar, {
    session: "session",
    query,
    showExpression: true,
    profile: "",
    onchange,
    onrun,
    onclose,
    oncancel,
    onsave: vi.fn(),
    onauthfailure: vi.fn(),
  });
  return { ...view, onchange, onrun, onclose, oncancel };
}

it("offers advanced search in a modal and lets Cancel discard the draft", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(response());
  const { oncancel } = open();
  expect(screen.getByRole("dialog", { name: "Filters and sort" })).toBeTruthy();
  expect((screen.getByLabelText("Query syntax") as HTMLTextAreaElement).value).toBe(query.text);
  expect(screen.getByRole("combobox", { name: "Sort by: Size" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(oncancel).toHaveBeenCalledOnce();
});

it("preserves independent facets and sort while editing the entire expression", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(response());
  const { onchange } = open();
  await screen.findByRole("status", { name: "Query valid" });
  expect((screen.getByRole("button", { name: "Search" }) as HTMLButtonElement).disabled).toBe(
    false,
  );
  await fireEvent.input(screen.getByLabelText("Query syntax"), {
    target: { value: "NOT tag:missing" },
  });
  expect(onchange).toHaveBeenLastCalledWith({ ...query, text: "NOT tag:missing" });
  expect(screen.queryByRole("status", { name: "Query valid" })).toBeNull();
  expect((screen.getByRole("button", { name: "Save…" }) as HTMLButtonElement).disabled).toBe(false);
});

it("runs the complete locally valid draft with explicit snapshot options", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(response());
  const { onrun, rerender } = open();
  await rerender({
    profiles: [
      {
        name: "archive",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: [],
        query_embedding_bindings: [],
        reranking_available: false,
      },
      {
        name: "default",
        fingerprint: "b".repeat(64),
        rendition: true,
        embedding_bindings: [],
        query_embedding_bindings: [],
        reranking_available: false,
      },
    ],
  });
  await fireEvent.click(screen.getByRole("combobox", { name: "Processing profile: Default" }));
  await fireEvent.click(screen.getByRole("option", { name: "archive" }));
  await fireEvent.click(screen.getByRole("combobox", { name: "Documents per page: 100" }));
  await fireEvent.click(screen.getByRole("option", { name: "50" }));
  await fireEvent.input(screen.getByLabelText("Query syntax"), {
    target: { value: "report AND NOT tag:missing" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  expect(onrun).toHaveBeenCalledWith(
    { ...query, text: "report AND NOT tag:missing" },
    {
      profile: "archive",
      page_size: 50,
      facets: [
        "collections",
        "tags",
        "media_family",
        "extension",
        "modified",
        "size",
        "text_coverage",
        "duplicates",
      ],
    },
  );
});

it.each([
  { mode: "semantic", sort: query.sort, message: /unsupported search method/ },
  { mode: "hybrid", sort: query.sort, message: /unsupported search method/ },
  {
    mode: "lexical",
    sort: { field: "relevance", direction: "desc" },
    message: /Choose a sort other than Relevance/,
  },
])(
  "blocks unsupported snapshot execution for $mode/$sort.field",
  async ({ mode, sort, message }) => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (_input, init) =>
      mode === "lexical"
        ? response(String(init?.body))
        : new Response(
            JSON.stringify({
              code: "invalid_query",
              detail: "Unsupported query mode",
            }),
            { status: 422 },
          ),
    );
    const { onrun, rerender } = open();
    await rerender({ query: parseQuery(JSON.stringify({ ...query, mode, sort })) });
    const run = screen.getByRole("button", { name: "Search" }) as HTMLButtonElement;
    expect(run.disabled).toBe(true);
    expect(screen.getByText(message)).toBeTruthy();
    await fireEvent.click(run);
    expect(onrun).not.toHaveBeenCalled();
    if (mode === "lexical") await screen.findByRole("status", { name: "Query valid" });
    else await screen.findByText("Unsupported query mode");
    expect(run.disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Save…" }) as HTMLButtonElement).disabled).toBe(
      false,
    );

    await rerender({ query });
    expect(run.disabled).toBe(false);
  },
);

it("keeps invalid and duplicate facet JSON visible without dropping constraints", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(response());
  const { onchange } = open();
  for (const raw of ['{"size_min":', '{"size_min":2,"size_min":3}']) {
    await fireEvent.input(screen.getByLabelText("Filters JSON"), {
      target: { value: raw },
    });
    expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe(raw);
    expect(screen.getByRole("alert")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Save…" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
  }
  expect(onchange).not.toHaveBeenCalled();
});

it("shows a query length error beside the expression", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(response());
  open();
  const expression = screen.getByLabelText("Query syntax");
  await fireEvent.input(expression, { target: { value: "x".repeat(8193) } });
  expect(expression.getAttribute("aria-invalid")).toBe("true");
  expect(
    document.getElementById(expression.getAttribute("aria-describedby")!)?.textContent,
  ).toContain("8192");
});

it.each(["success", "invalid", "backend"])(
  "ignores an old %s after an edit before the next debounce",
  async (kind) => {
    let resolve!: (value: Response) => void;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(
      () =>
        new Promise<Response>((done) => {
          resolve = done;
        }),
    );
    open();
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
    const signal = fetch.mock.calls[0][1]?.signal;
    await fireEvent.input(screen.getByLabelText("Query syntax"), {
      target: { value: "new expression" },
    });
    expect(signal?.aborted).toBe(true);
    resolve(
      kind === "success"
        ? response()
        : new Response(
            JSON.stringify({
              code: kind === "invalid" ? "invalid_query" : "internal",
              detail: "Old failure",
              position: { offset: 0, end: 4 },
            }),
            { status: kind === "invalid" ? 422 : 500 },
          ),
    );
    await new Promise((done) => setTimeout(done, 30));
    await tick();
    expect(screen.queryByText(/Query validated|Old failure/)).toBeNull();
    expect((screen.getByLabelText("Query syntax") as HTMLTextAreaElement).value).toBe(
      "new expression",
    );
  },
);

it("selects a server byte span correctly without stealing focus during validation", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        code: "invalid_query",
        detail: "Unknown operand",
        position: { offset: 4, end: 5 },
      }),
      { status: 422 },
    ),
  );
  const { rerender } = open();
  await rerender({ query: { ...query, text: "😀x" } });
  await screen.findByText("Unknown operand");
  const expression = screen.getByLabelText("Query syntax") as HTMLTextAreaElement;
  await fireEvent.click(screen.getByRole("button", { name: "Focus query error" }));
  expect(document.activeElement).toBe(expression);
  expect([expression.selectionStart, expression.selectionEnd]).toEqual([2, 3]);
});

it("aborts pending validation on close and unmount", async () => {
  let resolve!: (value: Response) => void;
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(
    () =>
      new Promise<Response>((done) => {
        resolve = done;
      }),
  );
  const { onclose, unmount } = open();
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
  await fireEvent.keyDown(window, { key: "Escape" });
  expect(fetch.mock.calls[0][1]?.signal?.aborted).toBe(true);
  expect(onclose).toHaveBeenCalledOnce();
  unmount();
  resolve(response());
  await tick();
  expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull();
});

it("keeps a newer success when an older failure arrives after it", async () => {
  let old!: (value: Response) => void;
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          old = resolve;
        }),
    )
    .mockImplementation(async (_input, init) => response(String(init?.body)));
  open();
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
  await fireEvent.input(screen.getByLabelText("Query syntax"), {
    target: { value: "new expression" },
  });
  await screen.findByRole("status", { name: "Query valid" });
  old(
    new Response(JSON.stringify({ code: "internal", detail: "Obsolete failure" }), { status: 500 }),
  );
  await new Promise((resolve) => setTimeout(resolve, 30));
  expect(screen.getByRole("status", { name: "Query valid" })).toBeTruthy();
  expect(screen.queryByText("Obsolete failure")).toBeNull();
});

it("invalidates the old session before accepting a new preview", async () => {
  let old!: (value: Response) => void;
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          old = resolve;
        }),
    )
    .mockImplementation(async (_input, init) => response(String(init?.body)));
  const { rerender } = open();
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
  await rerender({ session: "replacement" });
  expect(fetch.mock.calls[0][1]?.signal?.aborted).toBe(true);
  old(
    new Response(JSON.stringify({ code: "invalid_query", detail: "Old session" }), { status: 422 }),
  );
  await screen.findByRole("status", { name: "Query valid" });
  expect(screen.queryByText("Old session")).toBeNull();
  expect(new Headers(fetch.mock.calls[1][1]?.headers).get("X-Docbank-Web-Session")).toBe(
    "replacement",
  );
});

it("runs top-bar text with filters and the accepted page size", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(response());
  const onrun = vi.fn();
  render(QueryBar, {
    session: "session",
    query,
    initialPageSize: 50,
    onrun,
    onchange: vi.fn(),
    onsave: vi.fn(),
    onclose: vi.fn(),
    oncancel: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":10}' },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  expect(onrun).toHaveBeenCalledWith(
    { ...query, filters: { size_min: 10 } },
    expect.objectContaining({ page_size: 50 }),
  );
});
