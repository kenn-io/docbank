import { createHash } from "node:crypto";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import SavedQueriesDrawer from "./SavedQueriesDrawer.svelte";
import { parseQuery } from "./query.js";

const canonical = '{"filters":{"extensions":["pdf"],"no_tags":true},"mode":"hybrid","sort":{"direction":"desc","field":"size"},"syntax":"advanced","text":"alpha OR beta","v":1}';
const record = {
  id: "11111111-1111-4111-8111-111111111111", name: "Review PDFs", description: "Synthetic query",
  kind: "query", payload: JSON.parse(canonical), revision: 2,
  fingerprint: `sha256:${createHash("sha256").update(canonical).digest("hex")}`,
  created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z",
};
const json = (value: unknown, revision = 2) => new Response(JSON.stringify(value), { headers: { ETag: `"${revision}"` } });

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

function open(onload = vi.fn()) {
  render(SavedQueriesDrawer, { session: "session", initialQuery: parseQuery("{}"), onload, onclose: vi.fn(), onauthfailure: vi.fn() });
  return onload;
}

it("loads a complete definition and saves it without silently executing it", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) => {
    if (request?.method === "POST") {
      const body = JSON.parse(String(request.body));
      expect(body.payload).toEqual(JSON.parse(canonical));
      return json({ ...record, ...body, revision: 1 }, 1);
    }
    return json({ items: [record], total: 1, limit: 100, offset: 0 });
  });
  const onload = open();
  await fireEvent.click(await screen.findByRole("button", { name: "Edit Review PDFs" }));
  expect(JSON.parse((screen.getByLabelText("Complete query JSON") as HTMLTextAreaElement).value)).toEqual(JSON.parse(canonical));
  await fireEvent.click(screen.getByRole("button", { name: "Keep query draft" }));
  expect(onload).toHaveBeenCalledWith(parseQuery(canonical));
  onload.mockClear();
  expect(screen.getByText(/Saved-query execution is unavailable/)).toBeTruthy();
  await fireEvent.input(screen.getByLabelText("Definition name"), { target: { value: "Copy of review" } });
  await fireEvent.click(screen.getByRole("button", { name: "Save as new" }));
  await screen.findByText(/Saved Copy of review/);
  expect(onload).not.toHaveBeenCalled();
  expect(fetch.mock.calls.some(([url]) => String(url).includes("/search"))).toBe(false);
});

it("preserves an unrelated unsaved editor when deleting a definition", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) =>
    request?.method === "DELETE" ? json(record)
      : json({ items: [record], total: 1, limit: 100, offset: 0 }));
  open();
  await fireEvent.input(screen.getByLabelText("Definition name"), { target: { value: "Unfinished query" } });
  await fireEvent.input(screen.getByLabelText("Complete query JSON"), { target: { value: canonical } });
  await fireEvent.click(await screen.findByRole("button", { name: "Delete Review PDFs" }));
  await fireEvent.click(screen.getByRole("button", { name: "Confirm deletion" }));
  await screen.findByText(/^Deleted Review PDFs/);
  expect((screen.getByLabelText("Definition name") as HTMLInputElement).value).toBe("Unfinished query");
  expect((screen.getByLabelText("Complete query JSON") as HTMLTextAreaElement).value).toBe(canonical);
});

it.each(["Save as new", "Save changes"])("reloads committed NFC-divergent names after %s before another save", async (action) => {
  let current = record;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) => {
    if (request?.method) {
      current = { ...record, name: "\u1ea0", revision: action === "Save as new" ? 1 : 3 };
      return json(current, current.revision);
    }
    return json({ items: [current], total: 1, limit: 100, offset: 0 });
  });
  open();
  await fireEvent.click(await screen.findByRole("button", { name: "Edit Review PDFs" }));
  await fireEvent.input(screen.getByLabelText("Definition name"), { target: { value: "\u{20041}\u0323" } });
  await fireEvent.click(screen.getByRole("button", { name: action }));
  await screen.findByRole("alert");
  await screen.findByRole("button", { name: "Edit \u1ea0" });
  expect((screen.getByRole("button", { name: "Save changes" }) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("button", { name: "Save as new" }) as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Edit \u1ea0" }));
  expect((screen.getByLabelText("Definition name") as HTMLInputElement).value).toBe("\u1ea0");
  expect(screen.getByText(new RegExp(`Revision ${current.revision} ·`))).toBeTruthy();
  expect((screen.getByRole("button", { name: "Save changes" }) as HTMLButtonElement).disabled).toBe(false);
});

it("keeps stale rename and named revision-bound deletion failures visible", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) => {
    if (request?.method) return new Response(JSON.stringify({ detail: "Changed elsewhere; reload the definition", code: "stale_revision" }), { status: 412 });
    return json({ items: [record], total: 1, limit: 100, offset: 0 });
  });
  open();
  await fireEvent.click(await screen.findByRole("button", { name: "Edit Review PDFs" }));
  await fireEvent.input(screen.getByLabelText("Definition name"), { target: { value: "Renamed" } });
  await fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Changed elsewhere");
  await fireEvent.click(screen.getByRole("button", { name: "Delete Review PDFs" }));
  expect(screen.getByText(/Revision 2/)).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Confirm deletion" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Changed elsewhere");
  expect(screen.queryByText(/^Deleted /)).toBeNull();
  const mutations = fetch.mock.calls.filter(([, request]) => request?.method);
  expect(mutations).toHaveLength(2);
  expect(new Headers(mutations[1][1]?.headers).get("If-Match")).toBe('"2"');
});

it("edits bounded highlight terms without offering execution", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(json({ items: [], total: 0, limit: 100, offset: 0 }));
  open();
  await fireEvent.click(screen.getByRole("button", { name: "New highlight set" }));
  await fireEvent.input(screen.getByLabelText("Definition name"), { target: { value: "Key terms" } });
  await fireEvent.input(screen.getByLabelText("Term 1"), { target: { value: "alpha" } });
  await fireEvent.input(screen.getByLabelText("Color 1"), { target: { value: "red" } });
  await fireEvent.click(screen.getByRole("button", { name: "Save as new" }));
  expect((await screen.findByRole("alert")).textContent).toContain("#rrggbb");
  expect(screen.queryByRole("button", { name: /run/i })).toBeNull();
  expect(screen.queryByRole("button", { name: "Keep query draft" })).toBeNull();
});

it("opens the exact saved payload through the explicit query-editor action", async () => {
  vi.spyOn(globalThis,"fetch").mockResolvedValue(json({items:[record],total:1,limit:100,offset:0}));
  const onopenquery = vi.fn();
  render(SavedQueriesDrawer,{session:"session",initialQuery:parseQuery("{}"),onload:vi.fn(),onopenquery,onclose:vi.fn(),onauthfailure:vi.fn()});
  await fireEvent.click(await screen.findByRole("button",{name:"Open query Review PDFs"}));
  expect(onopenquery).toHaveBeenCalledWith(JSON.parse(canonical));
});
