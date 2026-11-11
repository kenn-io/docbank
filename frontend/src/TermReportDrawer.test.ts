import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import TermReportDrawer from "./TermReportDrawer.svelte";
import { drawerPage } from "./drawerNavigation.js";

beforeEach(() => {
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
});
afterEach(() => Reflect.deleteProperty(Element.prototype, "scrollIntoView"));

const collectionID = "11111111-1111-4111-8111-111111111111";
const original = {
  version: 1,
  all_documents: true,
  timezone: "UTC",
  coverage_mode: "available_only",
  terms: [
    {
      number: 1,
      expression: "alpha",
      syntax: "simple",
      dates: { start: "2024-01-01", end: "2026-12-31" },
    },
  ],
};
const oldSummary = {
  id: "a".repeat(48),
  state: "complete",
  observed_at: "2026-09-20T12:00:00Z",
  expires_at: "2026-09-20T12:30:00Z",
  terms: original.terms,
  counts: [
    {
      hits: 1,
      hits_plus_family: 1,
      unique_hits: 1,
      unique_families: 1,
      unique_hits_plus_family: 1,
    },
  ],
  coverage: {
    scoped: 1,
    searchable: 1,
    missing_text: 0,
    incomplete_families: 0,
    fallback_dates: 0,
  },
  unresolved_dates: 0,
};
const collection = {
  id: collectionID,
  source_kind: "cli",
  source_description: "Synthetic import",
  started_at: "2026-09-20T12:00:00Z",
  file_count: 1,
  total_bytes: 12,
  label: "New source",
  label_revision: 1,
  label_updated_at: "2026-09-20T12:00:00Z",
};
const json = (value: unknown) =>
  new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  document.querySelector('meta[name="docbank-base-path"]')?.remove();
});

it.each(["", "/archive"])("downloads report CSV and bundles under mount %s", async (basePath) => {
  const meta = document.createElement("meta");
  meta.name = "docbank-base-path";
  meta.content = basePath;
  document.head.append(meta);
  const formats: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url);
    if (
      init?.method === "GET" &&
      (path.startsWith(`${basePath}/api/v1/collections?`) ||
        path.startsWith(`${basePath}/api/v1/search-exports?`))
    )
      return json({ items: [], total: 0 });
    if (init?.method === "POST" && path === `${basePath}/api/v1/search-exports`)
      return json(oldSummary);
    if (
      init?.method === "POST" &&
      path === `${basePath}/api/v1/search-exports/${oldSummary.id}/download`
    ) {
      const { format } = JSON.parse(String(init.body));
      formats.push(format);
      return json({ url: `/api/daemon/web-download/file?ticket=${format}-ticket` });
    }
    throw new Error(`Unexpected request: ${init?.method} ${path}`);
  });
  const clicked: (string | null)[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    clicked.push(this.getAttribute("href"));
  });
  render(TermReportDrawer, {
    session: "session",
    initialExpression: "alpha",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  const csv = await screen.findByRole("button", { name: "Download CSV" });
  await waitFor(() => expect(csv.hasAttribute("disabled")).toBe(false));
  await fireEvent.click(csv);
  await waitFor(() =>
    expect(clicked).toEqual([`${basePath}/api/daemon/web-download/file?ticket=csv-ticket`]),
  );
  const bundle = screen.getByRole("button", { name: "Download evidence ZIP" });
  await waitFor(() => expect(bundle.hasAttribute("disabled")).toBe(false));
  await fireEvent.click(bundle);
  await waitFor(() =>
    expect(clicked).toEqual([
      `${basePath}/api/daemon/web-download/file?ticket=csv-ticket`,
      `${basePath}/api/daemon/web-download/file?ticket=bundle-ticket`,
    ]),
  );
  expect(formats).toEqual(["csv", "bundle"]);
});

it("merges continued date evidence into one document card and retains its choice", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const document = { node_id: 1, version_id: "v1", sha256: "a".repeat(64) };
  const laterDocument = { node_id: 2, version_id: "v2", sha256: "b".repeat(64) };
  const candidate = {
    id: "date-1",
    document,
    role: "document_date",
    raw: "2026-01-15",
    source_class: "content",
    locator: { evidence_sha256: "c".repeat(64), start_byte: 0, end_byte: 10 },
  };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url);
    if (path.startsWith("/api/v1/collections?") || init?.method === "GET")
      return json({ items: [], total: 0 });
    if (path.endsWith("/dates")) {
      const { cursor } = JSON.parse(String(init?.body));
      return cursor
        ? json({
            members: [
              {
                document,
                selection: {},
                candidates_complete: true,
                candidates: [{ ...candidate, id: "date-2", raw: "2026-02-16" }],
              },
              {
                document: laterDocument,
                selection: {},
                candidates_complete: true,
                candidates: [
                  { ...candidate, id: "date-3", document: laterDocument, raw: "2026-03-17" },
                ],
              },
            ],
          })
        : json({
            members: [
              { document, selection: {}, candidates_complete: false, candidates: [candidate] },
            ],
            next_cursor: "continued-document",
          });
    }
    return json({ ...oldSummary, state: "needs_review", unresolved_dates: 2 });
  });
  render(TermReportDrawer, {
    session: "session",
    initialExpression: "alpha",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  const review = await screen.findByRole("button", { name: "Review dates" });
  await waitFor(() => expect(review.hasAttribute("disabled")).toBe(false));
  await fireEvent.click(review);
  await fireEvent.click(await screen.findByRole("radio", { name: /2026-01-15/ }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Reason" }), {
    target: { value: "Checked the first source date" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Load more dates" }));
  expect(await screen.findByText("Document 2")).toBeTruthy();
  expect(screen.getAllByText("Document 1")).toHaveLength(1);
  const firstCard = screen.getByRole("group", { name: "Document 1" });
  expect(within(firstCard).getByRole("radio", { name: /2026-01-15/ })).toHaveProperty(
    "checked",
    true,
  );
  expect(within(firstCard).getByRole("radio", { name: /2026-02-16/ })).toBeTruthy();
  expect(within(firstCard).getByRole("textbox", { name: "Reason" })).toHaveProperty(
    "value",
    "Checked the first source date",
  );
  expect(screen.getByRole("radio", { name: /2026-03-17/ })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Load more dates" })).toBeNull();
});

it.each([false, true])(
  "preserves date interpretation evidence (rendition=%s)",
  async (rendition) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    const document = { node_id: 1, version_id: "v1", sha256: "a".repeat(64) };
    const candidate = {
      id: "date-1",
      document,
      role: "document_date",
      raw: rendition ? String.raw`01\/02\/2026` : "01/02/2026",
      source_class: "content",
      rejection: "ambiguous_numeric_date",
      locator: {
        rendition_id: rendition ? "retained-markdown" : undefined,
        evidence_sha256: "b".repeat(64),
        start_byte: 0,
        end_byte: rendition ? 12 : 10,
      },
    };
    const revisions: { choices: unknown[] }[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
      const path = String(url);
      if (path.startsWith("/api/v1/collections?")) return json({ items: [], total: 0 });
      if (init?.method === "GET") return json({ items: [], total: 0 });
      if (path.endsWith("/dates"))
        return json({
          members: [
            {
              document,
              selection: {},
              candidates_complete: true,
              candidates: [
                candidate,
                { ...candidate, id: "date-2", raw: "2026-02-31", rejection: "invalid_date" },
                {
                  ...candidate,
                  id: "date-3",
                  raw: rendition ? String.raw`2026\-01\-15` : "2026-01-15",
                  rejection: "",
                },
              ],
            },
          ],
        });
      if (path.endsWith("/revisions")) {
        revisions.push(JSON.parse(String(init?.body)));
        return json(oldSummary);
      }
      return json({ ...oldSummary, state: "needs_review", unresolved_dates: 1 });
    });
    render(TermReportDrawer, {
      session: "session",
      initialExpression: "alpha",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
    const review = await screen.findByRole("button", { name: "Review dates" });
    await waitFor(() => expect(review.hasAttribute("disabled")).toBe(false));
    await fireEvent.click(review);
    await fireEvent.click(await screen.findByRole("radio", { name: /01\/02\/2026/ }));
    await fireEvent.input(screen.getByRole("textbox", { name: "Reason" }), {
      target: { value: "Checked the source date" },
    });
    await fireEvent.click(
      screen.getByRole("button", { name: "Create report with these date choices" }),
    );
    expect(await screen.findByRole("alert")).toHaveProperty(
      "textContent",
      "Enter a reviewed date and source timezone for each interpreted date.",
    );
    expect(revisions).toHaveLength(0);
    expect(screen.getByRole("radio", { name: /2026-02-31/ }).hasAttribute("disabled")).toBe(true);
    await fireEvent.input(screen.getByRole("textbox", { name: "Reviewed date" }), {
      target: { value: "2026-01-02" },
    });
    await fireEvent.input(screen.getByRole("textbox", { name: "Source timezone" }), {
      target: { value: "UTC" },
    });
    await fireEvent.click(
      screen.getByRole("button", { name: "Create report with these date choices" }),
    );
    await waitFor(() => expect(revisions).toHaveLength(1));
    expect(revisions[0].choices[0]).toMatchObject({
      action: "interpret",
      reviewed_date: "2026-01-02",
      reviewed_timezone: "UTC",
      candidate_id: "date-1",
      evidence_sha256: "b".repeat(64),
    });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Review dates" }).hasAttribute("disabled")).toBe(
        false,
      ),
    );
    await fireEvent.click(screen.getByRole("button", { name: "Review dates" }));
    await fireEvent.click(await screen.findByRole("radio", { name: /2026-01-15/ }));
    expect(screen.queryByRole("option", { name: "Interpret source date" })).toBeNull();
    await fireEvent.input(screen.getByRole("textbox", { name: "Reason" }), {
      target: { value: "Retain source date" },
    });
    await fireEvent.click(screen.getByRole("combobox", { name: /^Action/ }));
    await fireEvent.click(screen.getByRole("option", { name: "Reclassify role" }));
    await fireEvent.click(screen.getByRole("combobox", { name: /^Reviewed role/ }));
    await fireEvent.click(screen.getByRole("option", { name: "created" }));
    await fireEvent.click(screen.getByRole("combobox", { name: /^Action/ }));
    await fireEvent.click(screen.getByRole("option", { name: "Select source date" }));
    await fireEvent.click(
      screen.getByRole("button", { name: "Create report with these date choices" }),
    );
    await waitFor(() => expect(revisions).toHaveLength(2));
    expect(revisions[1].choices[0]).toMatchObject({ action: "select" });
    expect(revisions[1].choices[0]).not.toHaveProperty("reviewed_role");
  },
);

it("loads a prior request, changes its source, and sends a fresh run without old review choices", async () => {
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
  const submitted: unknown[] = [];
  let datesLoaded = false;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url);
    if (path.startsWith("/api/v1/collections?"))
      return json({ items: [collection], total: 1, limit: 100, offset: 0 });
    if (path.startsWith("/api/v1/search-exports?") && init?.method === "GET") {
      return json({
        items: [
          { request: { ...original, date_choices: [{ action: "select" }] }, summary: oldSummary },
        ],
        total: 1,
      });
    }
    if (path === "/api/v1/search-exports" && init?.method === "POST") {
      submitted.push(JSON.parse(String(init.body)));
      return json({ ...oldSummary, id: "b".repeat(48), observed_at: "2026-09-20T13:00:00Z" });
    }
    if (path.endsWith("/dates") && init?.method === "POST") {
      datesLoaded = true;
      return json({ members: [], next_cursor: "" });
    }
    throw new Error(`unexpected request ${path}`);
  });
  render(TermReportDrawer, {
    session: "session",
    initialExpression: "",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Use as draft" }));
  await fireEvent.click(screen.getByRole("radio", { name: "Selected imports" }));
  await fireEvent.click(await screen.findByRole("checkbox", { name: /New source/ }));
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  await waitFor(() => expect(submitted).toHaveLength(1));
  expect(submitted[0]).toMatchObject({
    all_documents: false,
    collection_ids: [collectionID],
    timezone: "UTC",
    terms: original.terms,
    date_choices: [],
  });
  expect(submitted[0]).not.toHaveProperty("counts");
  const preview = await screen.findByRole("table", { name: "Search report counts" });
  expect(within(preview).getByText("Terms")).toBeTruthy();
  expect(within(preview).getByText("Date range")).toBeTruthy();
  const reviewButton = screen.getByRole("button", { name: "Review dates" });
  await waitFor(() => expect(reviewButton.hasAttribute("disabled")).toBe(false));
  await fireEvent.click(reviewButton);
  await waitFor(() => expect(datesLoaded).toBe(true));
  await fireEvent.input(screen.getByRole("textbox", { name: "Expression for term 1" }), {
    target: { value: "beta" },
  });
  expect(await screen.findByText(/These counts use your previous search/)).toBeTruthy();
});

it("keeps selected history fixed, clears review choices, and explains date-scoped counts and conflicts", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const documents = [
    { node_id: 1, version_id: "20000000-0000-4000-8000-000000000001", sha256: "a".repeat(64) },
    { node_id: 2, version_id: "20000000-0000-4000-8000-000000000002", sha256: "b".repeat(64) },
  ];
  const submitted: unknown[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    if (String(url).startsWith("/api/v1/collections?")) return json({ items: [], total: 0 });
    if (init?.method === "GET")
      return json({
        items: [
          {
            request: {
              ...original,
              all_documents: false,
              selected_documents: { documents },
              date_choices: [{ action: "select" }],
            },
            summary: oldSummary,
          },
        ],
        total: 1,
      });
    submitted.push(JSON.parse(String(init?.body)));
    return submitted.length === 1
      ? json(oldSummary)
      : new Response(
          JSON.stringify({ code: "report_selection_changed", detail: "Selection changed" }),
          { status: 409, headers: { "Content-Type": "application/problem+json" } },
        );
  });
  render(TermReportDrawer, {
    session: "session",
    initialExpression: "",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Use as draft" }));
  expect(screen.getAllByText(/Selected documents \(2\)/).length).toBeGreaterThan(0);
  expect(screen.queryByRole("radio", { name: "All documents" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  await waitFor(() => expect(submitted).toHaveLength(1));
  expect(submitted[0]).toMatchObject({
    all_documents: false,
    selected_documents: { documents },
    date_choices: [],
  });
  expect(await screen.findByText(/1 in date ranges · 1 searchable/)).toBeTruthy();
  await fireEvent.input(screen.getByRole("textbox", { name: "Expression for term 1" }), {
    target: { value: "beta" },
  });
  expect(await screen.findByText(/These counts use your previous search/)).toBeTruthy();
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Create report" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  const conflict = await screen.findByRole("region", { name: "Selected documents changed" });
  expect(within(conflict).getByRole("status").textContent).toContain(
    "The report didn't run. Some selected documents have a different version or are no longer available.",
  );
  expect(submitted).toHaveLength(2);
});

it("offers Remove only when more than one term exists", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async () =>
    json({ items: [], total: 0, limit: 20, offset: 0 }),
  );
  render(TermReportDrawer, {
    session: "session",
    initialExpression: "alpha",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  expect(screen.queryByRole("button", { name: /^Remove term/ })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Add term" }));
  await fireEvent.click(screen.getByRole("button", { name: "Remove term 1" }));
  expect(screen.queryByRole("button", { name: /^Remove term/ })).toBeNull();
  expect(
    (screen.getByRole("textbox", { name: "Expression for term 2" }) as HTMLInputElement).value,
  ).toBe("");
});

it.each(["beta", ""])(
  "starts a fresh report after canceling a two-term history draft with current search %j",
  async (expression) => {
    const request = {
      ...original,
      all_documents: false,
      collection_ids: [collectionID],
      timezone: "America/New_York",
      terms: [...original.terms, { ...original.terms[0], number: 2, expression: "omega" }],
    };
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      if (String(input).startsWith("/api/v1/search-exports?"))
        return json({
          items: [{ request, summary: { ...oldSummary, terms: request.terms } }],
          total: 1,
        });
      return json({ items: [collection], total: 1 });
    });
    const { rerender } = render(TermReportDrawer, {
      props: {
        session: "session",
        initialExpression: "alpha",
        onclose: vi.fn(),
        onauthfailure: vi.fn(),
      },
      context: new Map([[drawerPage, true]]),
    });
    await fireEvent.click(await screen.findByRole("button", { name: "Use as draft" }));
    expect(screen.getByRole("textbox", { name: "Expression for term 2" })).toHaveProperty(
      "value",
      "omega",
    );
    expect(screen.getByText(/^Loaded the /)).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText(/^Loaded the /)).toBeNull();
    await rerender({ initialExpression: expression });
    await fireEvent.click(screen.getByRole("button", { name: "Run report" }));
    expect(screen.getByRole("textbox", { name: "Expression for term 1" })).toHaveProperty(
      "value",
      expression,
    );
    expect(screen.queryByRole("textbox", { name: "Expression for term 2" })).toBeNull();
    expect(screen.getByRole("radio", { name: "All documents" }).getAttribute("aria-checked")).toBe(
      "true",
    );
    expect(screen.getByRole("combobox", { name: /^Missing information:/ })).toHaveProperty(
      "textContent",
      expect.stringContaining("Stop report"),
    );
    expect(screen.getByRole("textbox", { name: "Report timezone" })).toHaveProperty("value", "UTC");
    expect(screen.queryByRole("checkbox", { name: /New source/ })).toBeNull();

    await fireEvent.input(screen.getByRole("textbox", { name: "Expression for term 1" }), {
      target: { value: "edited draft" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await rerender({ initialExpression: "gamma" });
    await fireEvent.click(screen.getByRole("button", { name: "Run report" }));
    expect(screen.getByRole("textbox", { name: "Expression for term 1" })).toHaveProperty(
      "value",
      "gamma",
    );
  },
);

it("defaults a new report to the UTC date when Run report is chosen", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-09-20T23:59:59Z"));
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => json({ items: [], total: 0 }));
  render(TermReportDrawer, {
    props: {
      session: "session",
      initialExpression: "alpha",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    },
    context: new Map([[drawerPage, true]]),
  });

  vi.setSystemTime(new Date("2026-09-21T00:00:01Z"));
  await fireEvent.click(screen.getByRole("button", { name: "Run report" }));
  expect(screen.getByRole("button", { name: "2000-01-01 - 2026-09-21" })).toBeTruthy();
});

it("loads imports for the Searches page only once a report is started", async () => {
  const fetchMock = vi
    .spyOn(globalThis, "fetch")
    .mockImplementation(async (input) =>
      String(input).startsWith("/api/v1/collections?")
        ? json({ items: [collection], total: 1, limit: 100, offset: 0 })
        : json({ items: [], total: 0 }),
    );
  const collectionReads = () =>
    fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/collections?"))
      .length;
  render(TermReportDrawer, {
    props: {
      session: "session",
      initialExpression: "alpha",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    },
    context: new Map([[drawerPage, true]]),
  });
  expect(await screen.findByText("No reports yet.")).toBeTruthy();
  expect(collectionReads()).toBe(0);

  await fireEvent.click(screen.getByRole("button", { name: "Run report" }));
  await fireEvent.click(screen.getByRole("radio", { name: "Selected imports" }));
  expect(await screen.findByRole("checkbox", { name: /New source/ })).toBeTruthy();
  expect(collectionReads()).toBe(1);
});

it("requires acceptance before using current document versions", async () => {
  const old = {
    node_id: 1,
    version_id: "20000000-0000-4000-8000-000000000001",
    sha256: "a".repeat(64),
  };
  const current = {
    ...old,
    version_id: "20000000-0000-4000-8000-000000000002",
    sha256: "b".repeat(64),
  };
  const trashed = { ...old, node_id: 2 };
  const missing = { ...old, node_id: 3 };
  const submissions: any[] = [];
  const onselectionchanged = vi.fn();
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    if (init?.method === "GET") return json({ items: [], total: 0 });
    if (String(url).endsWith("/selection"))
      return json([
        {
          selected: old,
          current,
          name: "changed.txt",
          modified: "2026-09-20T12:00:00Z",
          status: "changed",
        },
        { selected: trashed, name: "trash.txt", status: "trashed" },
        { selected: missing, status: "missing" },
      ]);
    submissions.push(JSON.parse(String(init?.body)));
    if (submissions.length > 1) return json(oldSummary);
    return new Response(
      JSON.stringify({ code: "report_selection_changed", detail: "Selection changed" }),
      { status: 409, headers: { "Content-Type": "application/problem+json" } },
    );
  });
  render(TermReportDrawer, {
    session: "session",
    initialExpression: "alpha",
    initialDocuments: [old, trashed, missing],
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
    onselectionchanged,
  });
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Refresh selection" }));
  expect(await screen.findByText("In trash")).toBeTruthy();
  expect(screen.getByText("No longer available")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Use this selection" })).toHaveProperty(
    "disabled",
    true,
  );
  const checkbox = screen.getByRole("checkbox", { name: "Include current version" });
  expect(checkbox).toHaveProperty("checked", false);
  await fireEvent.click(checkbox);
  await fireEvent.click(screen.getByRole("button", { name: "Use this selection" }));
  expect(onselectionchanged).toHaveBeenCalledOnce();
  expect(submissions).toHaveLength(1);
  expect(screen.getByText("Selection updated")).toBeTruthy();
  expect(
    screen.getByText(
      "1 document now uses its current version. 2 documents were left out. Create the report again.",
    ),
  ).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  await waitFor(() => expect(submissions).toHaveLength(2));
  expect(submissions[1].selected_documents.documents).toEqual([current]);
});

it("shows a selection conflict after editing a term", async () => {
  const document = {
    node_id: 1,
    version_id: "20000000-0000-4000-8000-000000000001",
    sha256: "a".repeat(64),
  };
  let finish!: (response: Response) => void;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) => {
    if (init?.method === "GET") return json({ items: [], total: 0 });
    return new Promise<Response>((resolve) => {
      finish = resolve;
    });
  });
  render(TermReportDrawer, {
    session: "session",
    initialExpression: "alpha",
    initialDocuments: [document],
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Expression for term 1" }), {
    target: { value: "beta" },
  });
  finish(
    new Response(
      JSON.stringify({ code: "report_selection_changed", detail: "Selection changed" }),
      { status: 409, headers: { "Content-Type": "application/problem+json" } },
    ),
  );
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Create report" })).toHaveProperty("disabled", false),
  );
  expect(screen.getByRole("button", { name: "Refresh selection" })).toBeTruthy();
});

it("rejects an unknown refresh status without changing the selection", async () => {
  const document = {
    node_id: 1,
    version_id: "20000000-0000-4000-8000-000000000001",
    sha256: "a".repeat(64),
  };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    if (init?.method === "GET") return json({ items: [], total: 0 });
    if (String(url).endsWith("/selection")) return json([{ selected: document, status: "future" }]);
    return new Response(JSON.stringify({ code: "report_selection_changed" }), {
      status: 409,
      headers: { "Content-Type": "application/problem+json" },
    });
  });
  render(TermReportDrawer, {
    session: "session",
    initialExpression: "alpha",
    initialDocuments: [document],
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Refresh selection" }));
  expect(await screen.findByText(/selection response was incomplete or invalid/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Use this selection" })).toBeNull();
});

it("shows paths and both version dates for matching document names", async () => {
  const one = {
    node_id: 1,
    version_id: "20000000-0000-4000-8000-000000000001",
    sha256: "a".repeat(64),
  };
  const two = { ...one, node_id: 2 };
  const current = { ...one, version_id: "20000000-0000-4000-8000-000000000002" };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    if (init?.method === "GET") return json({ items: [], total: 0 });
    if (String(url).endsWith("/selection"))
      return json(
        [one, two].map((selected, index) => ({
          selected,
          current: { ...current, node_id: selected.node_id },
          name: "file.txt",
          path: index ? "/two/file.txt" : "/one/file.txt",
          selected_recorded: "2026-01-01T00:00:00Z",
          modified: "2025-01-01T00:00:00Z",
          status: "changed",
        })),
      );
    return new Response(JSON.stringify({ code: "report_selection_changed" }), {
      status: 409,
      headers: { "Content-Type": "application/problem+json" },
    });
  });
  render(TermReportDrawer, {
    session: "session",
    initialExpression: "alpha",
    initialDocuments: [one, two],
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Refresh selection" }));
  expect(await screen.findByText("/one/file.txt")).toBeTruthy();
  expect(screen.getByText("/two/file.txt")).toBeTruthy();
  expect(screen.getAllByText(/^Selected: .*Current:/)).toHaveLength(2);
});

it("clears the conflict after a successful rerun", async () => {
  const document = {
    node_id: 1,
    version_id: "20000000-0000-4000-8000-000000000001",
    sha256: "a".repeat(64),
  };
  let attempts = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) => {
    if (init?.method === "GET") return json({ items: [], total: 0 });
    if (++attempts > 1) return json(oldSummary);
    return new Response(JSON.stringify({ code: "report_selection_changed" }), {
      status: 409,
      headers: { "Content-Type": "application/problem+json" },
    });
  });
  render(TermReportDrawer, {
    session: "session",
    initialExpression: "alpha",
    initialDocuments: [document],
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  await screen.findByRole("button", { name: "Refresh selection" });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Create report" })).toHaveProperty("disabled", false),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Create report" }));
  await screen.findByText("Counts ready", { exact: false });
  expect(screen.queryByRole("button", { name: "Refresh selection" })).toBeNull();
});
