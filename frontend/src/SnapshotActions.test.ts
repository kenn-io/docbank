import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
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

it("review_fresh_tag_run_offers_file_recovery", () => {
  render(SnapshotActions, props());

  expect(screen.getByRole("button", { name: "Continue" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Resume from file…" })).toBeTruthy();
  expect(screen.getByLabelText("Resume from a recovery file")).toBeTruthy();
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

it("preserves an existing assignment when following the tag dialog's undo advice", async () => {
  const assigned = new Set([7]);
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) => {
    const request = JSON.parse(String(init?.body)) as BatchTagRequest;
    const unchanged = request.nodes
      .filter((node) => assigned.has(node.node_id) === request.assign)
      .map((node) => node.node_id);
    for (const node of request.nodes) {
      if (request.assign) assigned.add(node.node_id);
      else assigned.delete(node.node_id);
    }
    return Response.json(await receiptFor(request, unchanged));
  });
  const view = render(SnapshotActions, props({ scope: "selection", count: 2, targets }));
  await chooseReview();
  const advice = screen.getByText(/Adds “Review” to 2 documents/).textContent;
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await screen.findByText("Added “Review” to 1 document. 1 already had it.");
  expect([...assigned]).toEqual([7, 9]);

  if (advice?.includes("Undo with Remove")) {
    view.unmount();
    render(
      SnapshotActions,
      props({ scope: "selection", count: 2, targets: [targets[0], { node_id: 9, revision: 5 }] }),
    );
    await chooseReview();
    await fireEvent.click(screen.getByRole("radio", { name: "Remove" }));
    await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await screen.findByText("Removed “Review” from 2 documents.");
  }

  expect(assigned.has(7)).toBe(true);
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
    "The result is unknown. Check these documents' tags before trying again.",
  );
  expect((screen.getByRole("combobox", { name: "Tag" }) as HTMLInputElement).disabled).toBe(true);
  expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(true);
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
