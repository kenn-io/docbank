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

it("keeps matching deliveries distinct and downloads the chosen allocation", async () => {
  const other = {
    ...candidate,
    artifact_id: "55555555-5555-4555-8555-555555555555",
    allocation_id: "66666666-6666-4666-8666-666666666666",
  };
  const receipt = recoveryReceipt("bates-stamp/v1");
  receipt.artifact_id = other.artifact_id;
  receipt.allocation_id = other.allocation_id;
  receipt.allocation!.allocation_id = other.allocation_id;
  receipt.namespace = {
    ...receipt.namespace!,
    namespace_id: "77777777-7777-4777-8777-777777777777",
    prefix: "QUAL0",
    padding: 5,
  };
  receipt.allocation!.namespace_id = receipt.namespace.namespace_id;
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/candidates?")) return json({ items: [candidate, other] });
    if (url.endsWith(other.allocation_id)) return json(receipt);
    if (url.endsWith("/download"))
      return new Response(
        JSON.stringify({ detail: "Synthetic ticket refused", code: "not_found" }),
        { status: 404 },
      );
    return json({ items: [], total: 0 });
  });
  mount();
  await search();
  const rows = within(
    await screen.findByRole("list", { name: "Matching productions" }),
  ).getAllByRole("listitem");
  expect(rows).toHaveLength(2);
  expect(screen.queryByRole("button", { name: "Download PDF" })).toBeNull();
  await fireEvent.click(rows[1]!.querySelector("summary")!);
  expect(await screen.findByText("Stamped export")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Download PDF" }));
  await waitFor(() =>
    expect(
      fetcher.mock.calls.some(
        ([url]) => String(url) === `/api/v1/bates/exports/${other.allocation_id}/download`,
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

it("keeps lookup usable after a history failure and restarts an expired cursor", async () => {
  let reads = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/candidates?")) {
      reads++;
      if (url.includes("cursor="))
        return new Response(
          JSON.stringify({ detail: "Cursor expired", code: "document_cursor_expired" }),
          { status: 410 },
        );
      return json({
        items: [{ ...candidate, evidence_truncated: true, evidence_cursor: "evidence" }],
        next_cursor: "next",
      });
    }
    return new Response(JSON.stringify({ detail: "History unavailable" }), { status: 503 });
  });
  mount();
  await search();
  await fireEvent.click(await screen.findByText("Page 2 of 2"));
  expect(await screen.findByText("Showing some matching pages.")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  expect(await screen.findByText("Cursor expired")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Search again" }));
  await waitFor(() => expect(reads).toBe(3));
  expect(await screen.findByRole("list", { name: "Matching productions" })).toBeTruthy();
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

it("pages on request, keeps progress by the button and deduplicates artifacts across renewed cursors", async () => {
  const next = deferredResponse();
  const second = { ...candidate, artifact_id: "55555555-5555-4555-8555-555555555555" };
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/candidates?"))
      return url.includes("cursor=")
        ? next.promise
        : json({ items: [candidate], next_cursor: "position-with-old-time" });
    return json({ items: [], total: 0 });
  });
  mount();
  await search();
  const more = await screen.findByRole("button", { name: "Load more" });
  expect(fetcher.mock.calls.filter(([url]) => String(url).includes("/candidates?"))).toHaveLength(
    1,
  );
  await fireEvent.click(more);
  expect(await screen.findByRole("button", { name: "Loading more…" })).toBe(more);
  expect((more as HTMLButtonElement).disabled).toBe(true);
  expect(screen.queryByText("Finding productions…")).toBeNull();
  next.resolve(json({ items: [candidate, second], next_cursor: "position-with-new-time" }));
  await waitFor(() =>
    expect(
      within(screen.getByRole("list", { name: "Matching productions" })).getAllByRole("listitem"),
    ).toHaveLength(2),
  );
  expect((screen.getByRole("button", { name: "Load more" }) as HTMLButtonElement).disabled).toBe(
    false,
  );
  expect(fetcher.mock.calls.filter(([url]) => String(url).includes("/candidates?"))).toHaveLength(
    2,
  );
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
