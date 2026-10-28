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
  render(BatesExportDrawer, { mode: "productions", session: "session", onauthfailure, onclose });
  return { onauthfailure, onclose };
}
async function search(label = "QUAL000002") {
  await fireEvent.input(screen.getByRole("textbox", { name: "Bates number" }), {
    target: { value: label },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Find" }));
}

it("looks up a retained PDF without waiting for history and fetches details only on expansion", async () => {
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
  expect(fetcher.mock.calls.some(([url]) => String(url).endsWith(candidate.allocation_id))).toBe(
    false,
  );
  await fireEvent.click(within(list).getByText("QUAL000002"));
  expect(await screen.findByText("Numbered redaction")).toBeTruthy();
  expect(screen.getByText("QUAL", { exact: true })).toBeTruthy();
  expect(screen.getByText("None", { exact: true })).toBeTruthy();
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

it("pages only on request and refuses a cursor that does not advance", async () => {
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (String(input).includes("/candidates?"))
      return json({ items: [candidate], next_cursor: "next" });
    return json({ items: [], total: 0 });
  });
  mount();
  await search();
  await screen.findByRole("list", { name: "Matching productions" });
  expect(fetcher.mock.calls.filter(([url]) => String(url).includes("/candidates?"))).toHaveLength(
    1,
  );
  await fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  expect(await screen.findByText("The result cursor did not advance. Search again.")).toBeTruthy();
  expect(
    within(screen.getByRole("list", { name: "Matching productions" })).getAllByRole("listitem"),
  ).toHaveLength(1);
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
  await fireEvent.click(await screen.findByText("QUAL000002"));
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
  await fireEvent.click(await screen.findByText("QUAL000002"));
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

it("appends the next result page without duplicating an artifact", async () => {
  const second = { ...candidate, artifact_id: "55555555-5555-4555-8555-555555555555" };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/candidates?"))
      return json(
        url.includes("cursor=")
          ? { items: [candidate, second] }
          : { items: [candidate], next_cursor: "next" },
      );
    return json({ items: [], total: 0 });
  });
  mount();
  await search();
  await fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
  await waitFor(() =>
    expect(
      within(screen.getByRole("list", { name: "Matching productions" })).getAllByRole("listitem"),
    ).toHaveLength(2),
  );
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
});
