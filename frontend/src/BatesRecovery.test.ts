import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import BatesExportDrawer from "./BatesExportDrawer.svelte";
import {
  recoveryCandidate as candidate,
  recoveryReceipt,
  recoveryJSON as json,
} from "./bates-recovery.test-fixtures.js";

beforeEach(() => {
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
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});
function deferredResponse() {
  let resolve!: (value: Response) => void;
  const promise = new Promise<Response>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function mount() {
  const onauthfailure = vi.fn(),
    onclose = vi.fn();
  const view = render(BatesExportDrawer, {
    mode: "productions",
    session: "session",
    onauthfailure,
    onclose,
  });
  return { ...view, onauthfailure, onclose };
}
async function search(label = "QUAL000002") {
  await fireEvent.input(screen.getByRole("textbox", { name: "Bates number" }), {
    target: { value: label },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Find" }));
}

it("opens the only match without waiting for history and keeps recovery separate from export actions", async () => {
  const pending = deferredResponse();
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/candidates?")) return json({ items: [candidate] });
    if (url.endsWith(candidate.allocation_id)) return json(recoveryReceipt());
    return pending.promise;
  });
  mount();
  await search();
  const list = await screen.findByRole("list", { name: "Matching productions" });
  expect(within(list).getByText("Page 2 of 2")).toBeTruthy();
  expect(await screen.findByText("Numbered redaction")).toBeTruthy();
  expect(screen.getByText("QUAL000001–QUAL000002")).toBeTruthy();
  expect(within(list).queryByRole("button", { name: "Refresh receipt" })).toBeNull();
  expect(list.querySelector("details")?.open).toBe(true);
  expect(
    fetcher.mock.calls.filter(([url]) => String(url).endsWith(candidate.allocation_id)),
  ).toHaveLength(1);
  expect(screen.getByRole("button", { name: "Download PDF" })).toBeTruthy();
});

it("downloads the stamped candidate's allocation and reports a refused ticket", async () => {
  const receipt = recoveryReceipt("bates-stamp/v1");
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/candidates?")) return json({ items: [candidate] });
    if (url.endsWith(candidate.allocation_id)) return json(receipt);
    if (url.endsWith("/download"))
      return new Response(
        JSON.stringify({ detail: "Synthetic ticket refused", code: "not_found" }),
        { status: 404 },
      );
    return json({ items: [], total: 0 });
  });
  mount();
  await search();
  expect(await screen.findByText("Stamped export")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Download PDF" }));
  await waitFor(() =>
    expect(
      fetcher.mock.calls.some(
        ([url]) => String(url) === `/api/v1/bates/exports/${candidate.allocation_id}/download`,
      ),
    ).toBe(true),
  );
  expect(await screen.findByText("Synthetic ticket refused")).toBeTruthy();
});

it.each(["new search", "clear"])("discards an obsolete response after %s", async (action) => {
  const first = deferredResponse();
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = new URL(String(input), "http://localhost");
    if (url.pathname.endsWith("/candidates")) {
      if (url.searchParams.get("bates_label") === "QUAL000002") return first.promise;
      return json({ items: [] });
    }
    return json({ items: [], total: 0 });
  });
  mount();
  await search();
  if (action === "clear") await fireEvent.click(screen.getByRole("button", { name: "Clear" }));
  else await search("NONE000001");
  first.resolve(json({ items: [candidate] }));
  await waitFor(() =>
    expect(screen.queryByRole("list", { name: "Matching productions" })).toBeNull(),
  );
  expect(
    await screen.findByText(
      action === "clear" ? "No productions yet" : "No retained PDF matches “NONE000001”.",
    ),
  ).toBeTruthy();
});

it("reports failed search and closes on session expiry", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (String(input).includes("/candidates?"))
      return new Response(JSON.stringify({ detail: "Session expired" }), { status: 401 });
    return json({ items: [], total: 0 });
  });
  const callbacks = mount();
  await search();
  await waitFor(() => expect(callbacks.onauthfailure).toHaveBeenCalledOnce());
  expect(callbacks.onclose).toHaveBeenCalledOnce();
  expect(screen.queryByText(/No retained PDF matches/)).toBeNull();
  expect(screen.queryByRole("button", { name: "Search again" })).toBeNull();
});

it("keeps lookup usable after a history failure", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/candidates?")) return json({ items: [candidate] });
    if (url.endsWith(candidate.allocation_id)) return json(recoveryReceipt());
    if (url.startsWith("/api/v1/bates/exports?"))
      return new Response(JSON.stringify({ detail: "History unavailable" }), { status: 503 });
    return json({ items: [], total: 0 });
  });
  mount();
  await screen.findByText("Publication history is unavailable: History unavailable");
  await search();
  expect(await screen.findByText("Numbered redaction")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Download PDF" })).toBeTruthy();
});

it("cannot restore a cleared result through a late receipt", async () => {
  const detail = deferredResponse();
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/candidates?")) return json({ items: [candidate] });
    if (url.endsWith(candidate.allocation_id)) return detail.promise;
    return json({ items: [], total: 0 });
  });
  mount();
  await search();
  await waitFor(() =>
    expect(fetcher.mock.calls.some(([url]) => String(url).endsWith(candidate.allocation_id))).toBe(
      true,
    ),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Clear" }));
  detail.resolve(json(recoveryReceipt()));
  await screen.findByText("No productions yet");
  expect(screen.queryByRole("button", { name: "Download PDF" })).toBeNull();
});

it("keeps the production list available when an empty or oversized search is submitted", async () => {
  const fetcher = vi
    .spyOn(globalThis, "fetch")
    .mockImplementation(async () => json({ items: [], total: 0 }));
  mount();
  await screen.findByText("No productions yet");
  const field = screen.getByRole("textbox", { name: "Bates number" });
  const find = screen.getByRole("button", { name: "Find" }) as HTMLButtonElement;
  for (const value of ["", "A".repeat(267)]) {
    await fireEvent.input(field, { target: { value } });
    expect(find.disabled).toBe(true);
    await fireEvent.submit(field.closest("form")!);
    expect(screen.getByText("No productions yet")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Search again" })).toBeNull();
  }
  expect(fetcher.mock.calls.some(([url]) => String(url).includes("/candidates?"))).toBe(false);
  await fireEvent.input(field, { target: { value: "A".repeat(266) } });
  expect(find.disabled).toBe(false);
});

it("restores the loading status when search unmounts on a drawer mode change", async () => {
  const pending = deferredResponse();
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
    String(input).includes("/candidates?") ? json({ items: [] }) : pending.promise,
  );
  const view = mount();
  await search();
  await screen.findByText("No retained PDF matches “QUAL000002”.");
  expect(screen.queryByText("Loading…")).toBeNull();
  await view.rerender({ mode: "create" });
  expect(await screen.findByText("Loading…")).toBeTruthy();
});

it("warns about surrounding spaces without changing the exact search", async () => {
  const fetcher = vi
    .spyOn(globalThis, "fetch")
    .mockImplementation(async () => json({ items: [], total: 0 }));
  mount();
  await search(" QUAL000002 ");
  expect(
    await screen.findByText("No retained PDF matches “ QUAL000002 ”.", {
      trim: false,
      collapseWhitespace: false,
    }),
  ).toBeTruthy();
  expect(screen.getByText("Leading or trailing spaces are included in the search.")).toBeTruthy();
  expect(
    screen.getByText("Numbers must match exactly, including prefix and leading zeros."),
  ).toBeTruthy();
  const call = fetcher.mock.calls.find(([url]) => String(url).includes("/candidates?"));
  expect(new URL(String(call![0]), "http://localhost").searchParams.get("bates_label")).toBe(
    " QUAL000002 ",
  );
});
