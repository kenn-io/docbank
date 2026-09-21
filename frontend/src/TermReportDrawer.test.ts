import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import TermReportDrawer from "./TermReportDrawer.svelte";

const collectionID = "11111111-1111-4111-8111-111111111111";
const original = {
  version: 1, all_documents: true, timezone: "UTC", coverage_mode: "available_only",
  terms: [{ number: 1, expression: "alpha", syntax: "simple",
    dates: { start: "2024-01-01", end: "2026-12-31" } }],
};
const oldSummary = { id: "a".repeat(48), state: "complete", observed_at: "2026-09-20T12:00:00Z",
  expires_at: "2026-09-20T12:30:00Z", terms: original.terms, counts: [{ hits: 1,
    hits_plus_family: 1, unique_hits: 1, unique_families: 1, unique_hits_plus_family: 1 }],
  coverage: { scoped: 1, searchable: 1, missing_text: 0, incomplete_families: 0, fallback_dates: 0 }, unresolved_dates: 0 };
const collection = { id: collectionID, source_kind: "cli", source_description: "Synthetic import",
  started_at: "2026-09-20T12:00:00Z", file_count: 1, total_bytes: 12,
  label: "New source", label_revision: 1, label_updated_at: "2026-09-20T12:00:00Z" };
const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it("loads a prior request, changes its source, and sends a fresh run without old review choices", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const submitted: unknown[] = [];
  let datesLoaded = false;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url);
    if (path.startsWith("/api/v1/collections?")) return json({ items: [collection], total: 1, limit: 100, offset: 0 });
    if (path.startsWith("/api/v1/term-reports?") && init?.method === "GET") {
      return json({ items: [{ request: { ...original, date_choices: [{ action: "select" }] }, summary: oldSummary }], total: 1 });
    }
    if (path === "/api/v1/term-reports" && init?.method === "POST") {
      submitted.push(JSON.parse(String(init.body)));
      return json({ ...oldSummary, id: "b".repeat(48), observed_at: "2026-09-20T13:00:00Z" });
    }
    if (path.endsWith("/dates") && init?.method === "POST") {
      datesLoaded = true;
      return json({ members: [], next_cursor: "" });
    }
    throw new Error(`unexpected request ${path}`);
  });
  render(TermReportDrawer, { session: "session", initialExpression: "", onclose: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Use as draft" }));
  await fireEvent.click(screen.getByRole("radio", { name: "Selected collections" }));
  await fireEvent.click(await screen.findByRole("checkbox", { name: /New source/ }));
  await fireEvent.click(screen.getByRole("button", { name: "Create export" }));
  await waitFor(() => expect(submitted).toHaveLength(1));
  expect(submitted[0]).toMatchObject({ all_documents: false, collection_ids: [collectionID],
    timezone: "UTC", terms: original.terms, date_choices: [] });
  expect(submitted[0]).not.toHaveProperty("counts");
  const preview = await screen.findByRole("table", { name: "Search export counts" });
  expect(within(preview).getByText("Terms")).toBeTruthy();
  expect(within(preview).getByText("Date Range")).toBeTruthy();
  const reviewButton = screen.getByRole("button", { name: "Review dates" });
  await waitFor(() => expect(reviewButton.hasAttribute("disabled")).toBe(false));
  await fireEvent.click(reviewButton);
  await waitFor(() => expect(datesLoaded).toBe(true));
  await fireEvent.input(screen.getByRole("textbox", { name: "Expression for term 1" }),
    { target: { value: "beta" } });
  expect(await screen.findByText(/Draft changed; the counts and downloads below belong to the previous frozen run/)).toBeTruthy();
});
