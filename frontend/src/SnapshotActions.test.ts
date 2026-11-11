import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import SnapshotActions from "./SnapshotActions.svelte";
import { ACTION_MAX_BYTES } from "./actionRecovery.js";
import { batchTagRequestDigest, type BatchTagRequest } from "./batch-tags.js";

const catalog = [
  {
    id: "22222222-2222-4222-8222-222222222222",
    name: "Review",
    revision: 2,
    assignment_count: 12,
  },
];
const targets = [
  { node_id: 7, revision: 3 },
  { node_id: 9, revision: 4 },
];

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
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

function props(overrides: Record<string, unknown> = {}) {
  return {
    session: "session",
    scope: "query" as const,
    count: 101,
    catalog,
    disabled: false,
    onstart: vi.fn(),
    onimport: vi.fn(),
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
    ...overrides,
  };
}

async function chooseReview(): Promise<void> {
  await fireEvent.input(screen.getByRole("combobox", { name: "Tag" }), {
    target: { value: "Rev" },
  });
  await fireEvent.click(screen.getByRole("option", { name: "Review (12)" }));
}

async function receiptFor(request: BatchTagRequest, unchanged: number[] = []) {
  return {
    version: 1,
    operation_id: request.operation_id,
    request_digest: await batchTagRequestDigest(request),
    tag_id: request.tag_id,
    assign: request.assign,
    tag_revision: 3,
    assignment_count: 13,
    completed_at: "2026-09-11T13:00:00.000000000Z",
    nodes: request.nodes.map((node) => ({
      node_id: node.node_id,
      expected_revision: node.revision,
      revision: unchanged.includes(node.node_id) ? node.revision : node.revision + 1,
      changed: !unchanged.includes(node.node_id),
    })),
  };
}

it("checks each document independently and keeps its path through retry", async () => {
  const writes: BatchTagRequest[] = [];
  let reads = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (init?.method === "POST") {
      if (url.includes("preview"))
        return Response.json({ code: "stale_revision" }, { status: 409 });
      const request = JSON.parse(String(init.body)) as BatchTagRequest;
      writes.push(request);
      if (writes.length === 1) throw new TypeError("Lost response");
      return Response.json(await receiptFor(request));
    }
    const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
    if (url.includes("/tags?"))
      return Response.json({ items: [], total: 0, limit: 1000, offset: 0 });
    const revision = id === 7 && ++reads > 1 ? 11 : id === 7 ? 10 : 12;
    return Response.json({
      id,
      name: "report.pdf",
      path: id === 7 ? "/alpha/report.pdf" : "/beta/report.pdf",
      revision,
    });
  });
  render(SnapshotActions, props({ scope: "selection", count: 2, targets }));
  await chooseReview();
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Check tag results" }));
  const retry = await screen.findByRole("button", { name: "Add “Review” to 1 document" });
  expect(writes).toHaveLength(1);
  await fireEvent.click(screen.getByText("Review documents"));
  expect(
    within(screen.getByText("/alpha/report.pdf").closest("tr")!).getByText("Couldn't check"),
  ).toBeTruthy();
  expect(
    within(screen.getByText("/beta/report.pdf").closest("tr")!).getByText("Missing “Review”"),
  ).toBeTruthy();
  await fireEvent.click(retry);
  await screen.findByText("Has “Review”");
  expect(
    within(screen.getByText("/alpha/report.pdf").closest("tr")!).getByText("Couldn't check"),
  ).toBeTruthy();
  expect(
    within(screen.getByText("/beta/report.pdf").closest("tr")!).getByText("Has “Review”"),
  ).toBeTruthy();
  expect(writes[1].nodes).toEqual([{ node_id: 9, revision: 12 }]);
});

it("states what a whole-result change does before continuing", async () => {
  const onstart = vi.fn();
  render(SnapshotActions, props({ onstart }));

  expect(screen.getByText("Adds a tag to 101 documents.")).toBeTruthy();
  const next = screen.getByRole("button", { name: "Continue" }) as HTMLButtonElement;
  expect(next.disabled).toBe(true);
  await chooseReview();
  await screen.findByText("Adds “Review” to 101 documents.");
  await fireEvent.click(screen.getByRole("radio", { name: "Remove" }));
  expect(screen.getByText("Removes “Review” from 101 documents.")).toBeTruthy();
  await fireEvent.click(next);
  expect(onstart).toHaveBeenCalledExactlyOnceWith({
    scope: "query",
    tagID: catalog[0].id,
    assign: false,
  });
});

it("tags the selection captured at open and reports what changed", async () => {
  const sent: BatchTagRequest[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) => {
    const request = JSON.parse(String(init?.body)) as BatchTagRequest;
    sent.push(request);
    return Response.json(await receiptFor(request, [9]));
  });
  const onchanged = vi.fn();
  const onstart = vi.fn();
  const view = render(
    SnapshotActions,
    props({ scope: "selection", count: 2, targets, onchanged, onstart }),
  );
  expect(screen.queryByRole("button", { name: "Resume from file…" })).toBeNull();
  await view.rerender(props({ scope: "selection", count: 2, targets: [], onchanged, onstart }));
  await chooseReview();
  await screen.findByText("Adds “Review” to 2 documents.");
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));

  await screen.findByText("Added “Review” to 1 document. 1 already had it.");
  expect(sent).toHaveLength(1);
  expect(sent[0]).toMatchObject({ tag_id: catalog[0].id, assign: true, nodes: targets });
  expect(onchanged).toHaveBeenCalledOnce();
  expect(onstart).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: "Done" })).toBeTruthy();
});

it("refreshes observations and locks an unknown selection result", async () => {
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockRejectedValueOnce(new TypeError("Synthetic response loss"));
  const onunknown = vi.fn();
  render(SnapshotActions, props({ scope: "selection", count: 2, targets, onunknown }));
  await chooseReview();
  await fireEvent.click(await screen.findByRole("button", { name: "Apply" }));
  await screen.findByText(
    "Docbank couldn't confirm the tag change. Check the documents' tags before trying again.",
  );
  expect((screen.getByRole("combobox", { name: "Tag" }) as HTMLInputElement).disabled).toBe(true);
  expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  expect(screen.getByRole("button", { name: "Check tag results" })).toBeTruthy();
  expect(fetch).toHaveBeenCalledOnce();
  expect(onunknown).toHaveBeenCalledOnce();
});

it("reads a recovery file without starting an action", async () => {
  const onstart = vi.fn();
  const onimport = vi.fn();
  render(SnapshotActions, props({ onstart, onimport }));

  expect(screen.getByRole("button", { name: "Resume from file…" })).toBeTruthy();

  const file = new File([new Uint8Array([1, 2, 3])], "action.docbank-action.json", {
    type: "application/json",
  });
  await fireEvent.change(screen.getByLabelText("Resume from a recovery file"), {
    target: { files: [file] },
  });

  await waitFor(() => expect(onimport).toHaveBeenCalledOnce());
  expect(Array.from(onimport.mock.calls[0][0] as Uint8Array)).toEqual([1, 2, 3]);
  expect(onstart).not.toHaveBeenCalled();
});

it("discards a recovery file whose read finishes after the dialog closes", async () => {
  const onimport = vi.fn();
  const view = render(
    SnapshotActions,
    props({ count: 1, onimport, onclose: () => view.unmount() }),
  );
  let release!: (bytes: ArrayBuffer) => void;
  const reading = new Promise<ArrayBuffer>((resolve) => {
    release = resolve;
  });
  const file = new File(["{}"], "action.json", { type: "application/json" });
  const read = vi.spyOn(file, "arrayBuffer").mockReturnValue(reading);
  await fireEvent.change(screen.getByLabelText("Resume from a recovery file"), {
    target: { files: [file] },
  });
  expect(read).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  release(new ArrayBuffer(0));
  await reading;
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(onimport).not.toHaveBeenCalled();
});

it("rejects an oversized recovery file before reading it", async () => {
  const onimport = vi.fn();
  render(SnapshotActions, props({ count: 1, onimport }));
  const file = new File(["{}"], "oversized-action.json", { type: "application/json" });
  Object.defineProperty(file, "size", { value: ACTION_MAX_BYTES + 1 });
  const read = vi.spyOn(file, "arrayBuffer");
  await fireEvent.change(screen.getByLabelText("Resume from a recovery file"), {
    target: { files: [file] },
  });
  expect(read).not.toHaveBeenCalled();
  expect(onimport).not.toHaveBeenCalled();
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("128 MiB"));
});

it.each([true, false])(
  "checks current tag state before an explicit retry (assign=%s)",
  async (assign) => {
    const writes: BatchTagRequest[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = String(input);
      if (init?.method === "POST") {
        const request = JSON.parse(String(init.body)) as BatchTagRequest;
        writes.push(request);
        if (writes.length === 1) throw new TypeError("Lost response");
        return Response.json(await receiptFor(request));
      }
      if (url.includes("/tags?"))
        return Response.json({
          items: assign ? [] : catalog,
          total: assign ? 0 : 1,
          limit: 1000,
          offset: 0,
        });
      const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
      if (id === 7) throw new TypeError("Network error");
      return Response.json({ id, name: `Document ${id}`, revision: 12 });
    });
    render(SnapshotActions, props({ scope: "selection", count: 2, targets }));
    await chooseReview();
    if (!assign) await fireEvent.click(screen.getByRole("radio", { name: "Remove" }));
    await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Check tag results" }));
    const retry = await screen.findByRole("button", {
      name: assign ? "Add “Review” to 1 document" : "Remove “Review” from 1 document",
    });
    expect(writes).toHaveLength(1);
    await fireEvent.click(screen.getByText("Review documents"));
    expect(screen.getByText(assign ? "Missing “Review”" : "Still has “Review”")).toBeTruthy();
    await fireEvent.click(retry);
    await screen.findByText(assign ? "Has “Review”" : "Doesn't have “Review”");
    expect(screen.getByText("Couldn't check")).toBeTruthy();
    await screen.findByRole("button", { name: "Check again" });
    expect(screen.getByText("Document 7")).toBeTruthy();
    expect(screen.queryByText(assign ? "Missing “Review”" : "Still has “Review”")).toBeNull();
    expect(writes[1].nodes).toEqual([{ node_id: 9, revision: 12 }]);
    expect(writes[1].operation_id).not.toBe(writes[0].operation_id);
  },
);

it.each(["unknown", "rejected", "stale"])(
  "keeps the original selection after a %s retry",
  async (outcome) => {
    const writes: BatchTagRequest[] = [];
    let applied = false;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = String(input);
      if (init?.method === "POST") {
        const request = JSON.parse(String(init.body)) as BatchTagRequest;
        writes.push(request);
        if (writes.length === 1) throw new TypeError("Lost response");
        if (outcome === "unknown") {
          applied = true;
          throw new TypeError("Lost retry response");
        }
        return Response.json(
          {
            code: outcome === "stale" ? "stale_revision" : "invalid_request",
            detail: "Retry refused",
          },
          { status: 409 },
        );
      }
      if (url.includes("/tags?"))
        return Response.json({
          items: applied ? catalog : [],
          total: applied ? 1 : 0,
          limit: 1000,
          offset: 0,
        });
      const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
      if (id === 7) throw new TypeError("Network error");
      return Response.json({ id, name: `Document ${id}`, revision: id === 7 ? 10 : 12 });
    });
    render(SnapshotActions, props({ scope: "selection", count: 2, targets }));
    await chooseReview();
    await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Check tag results" }));
    await fireEvent.click(
      await screen.findByRole("button", { name: "Add “Review” to 1 document" }),
    );
    await waitFor(() => expect(writes).toHaveLength(2));
    expect(writes[1].nodes).toEqual([{ node_id: 9, revision: 12 }]);
    if (outcome === "rejected") {
      await screen.findByText("Retry refused");
      expect(screen.getByRole("button", { name: "Add “Review” to 1 document" })).toBeTruthy();
      expect(screen.getByText(/0 of 2 documents/)).toBeTruthy();
    } else {
      if (outcome === "stale")
        await screen.findByText("Some documents changed. Check their tags again.");
      await waitFor(() =>
        expect(screen.queryByRole("button", { name: "Add “Review” to 1 document" })).toBeNull(),
      );
      await fireEvent.click(await screen.findByRole("button", { name: "Check again" }));
      await screen.findByText(outcome === "unknown" ? /1 of 2 documents/ : /0 of 2 documents/);
    }
  },
);

it("pages checked documents and resets to the first page on another check", async () => {
  const many = Array.from({ length: 51 }, (_, i) => ({ node_id: i + 1, revision: 1 }));
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (init?.method === "POST") throw new TypeError("Lost response");
    const url = String(input);
    if (url.includes("/tags?"))
      return Response.json({ items: [], total: 0, limit: 1000, offset: 0 });
    const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
    return Response.json({ id, name: `File ${id}`, path: `/File ${id}`, revision: 2 });
  });
  render(SnapshotActions, props({ scope: "selection", count: 51, targets: many }));
  await chooseReview();
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Check tag results" }));
  await screen.findByRole("button", { name: "Add “Review” to 51 documents" });
  await fireEvent.click(screen.getByText("Review documents"));
  expect(screen.getByText("1–50 of 51")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Next" }));
  expect(screen.getByText("51–51 of 51")).toBeTruthy();
  expect(screen.getByText("/File 51")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Check again" }));
  await screen.findByText("1–50 of 51");
});
