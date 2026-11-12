import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import SnapshotActions from "./SnapshotActions.svelte";
import { ACTION_MAX_BYTES } from "./actionRecovery.js";
import type { BatchTagRequest } from "./batch-tags.js";
import { receiptFor } from "./test/receiptFor.js";

const catalog = [
  {
    id: "22222222-2222-4222-8222-222222222222",
    name: "Review",
    revision: 2,
    assignment_count: 12,
  },
];
const targets = [
  { node_id: 7, revision: 3, name: "File 7" },
  { node_id: 9, revision: 4, name: "File 9" },
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
  expect(sent[0]).toMatchObject({
    tag_id: catalog[0].id,
    assign: true,
    nodes: [
      { node_id: 7, revision: 3 },
      { node_id: 9, revision: 4 },
    ],
  });
  expect(onchanged).toHaveBeenCalledOnce();
  expect(onstart).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: "Done" })).toBeTruthy();
});

it("reports an uncertain outcome on close", async () => {
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockRejectedValueOnce(new TypeError("Synthetic response loss"));
  const onclose = vi.fn();
  render(SnapshotActions, props({ scope: "selection", count: 2, targets, onclose }));
  await chooseReview();
  await fireEvent.click(await screen.findByRole("button", { name: "Apply" }));
  await screen.findByText(
    "Docbank couldn't confirm the tag change. Check the documents' tags before trying again.",
  );
  expect((screen.getByRole("combobox", { name: "Tag" }) as HTMLInputElement).disabled).toBe(true);
  expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  expect(screen.queryByRole("button", { name: /Add “Review” to/ })).toBeNull();
  expect(screen.getByRole("button", { name: "Check tag results" })).toBeTruthy();
  expect(fetch).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: "Close" }));
  expect(onclose).toHaveBeenCalledWith(true);
});

it("reports an expired session while checking", async () => {
  vi.spyOn(globalThis, "fetch")
    .mockRejectedValueOnce(new TypeError("Lost response"))
    .mockResolvedValue(Response.json({ detail: "expired" }, { status: 401 }));
  const onauthfailure = vi.fn();
  render(SnapshotActions, props({ scope: "selection", count: 2, targets, onauthfailure }));
  await chooseReview();
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Check tag results" }));
  await waitFor(() => expect(onauthfailure).toHaveBeenCalledOnce());
});

it.each(["stale_revision", "not_found"])(
  "requires a refreshed selection after %s",
  async (code) => {
    const fetch = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation(async (_input, init) =>
        init?.method === "POST"
          ? Response.json({ code }, { status: code === "not_found" ? 404 : 409 })
          : Response.json(catalog[0]),
      );
    render(SnapshotActions, props({ scope: "selection", count: 2, targets }));
    await chooseReview();
    await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await screen.findByText("Some documents changed. Close, refresh, and select them again.");
    expect((screen.getByRole("combobox", { name: "Tag" }) as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByRole("button", { name: "Close" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Check/ })).toBeNull();
    expect(fetch).toHaveBeenCalledTimes(1);
  },
);

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

it("cancels an in-progress check when the dialog closes", async () => {
  let signal: AbortSignal | null | undefined;
  vi.spyOn(globalThis, "fetch")
    .mockRejectedValueOnce(new TypeError("Lost response"))
    .mockImplementation((_input, init) => {
      signal = init?.signal;
      return new Promise((_resolve, reject) => {
        signal?.addEventListener("abort", () => reject(signal?.reason));
      });
    });
  const view = render(
    SnapshotActions,
    props({ scope: "selection", count: 2, targets, onclose: () => view.unmount() }),
  );
  await chooseReview();
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Check tag results" }));
  await fireEvent.click(screen.getByRole("button", { name: "Close" }));
  expect(signal?.aborted).toBe(true);
});

it.each(["unknown", "rejected", "stale"])(
  "offers another check after a %s retry",
  async (outcome) => {
    const writes: BatchTagRequest[] = [];
    let applied = false;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === "/api/v1/batch/tags") {
        const request = JSON.parse(String(init?.body)) as BatchTagRequest;
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
      if (url.endsWith(`/tags/${catalog[0].id}`)) return Response.json(catalog[0]);
      if (url.endsWith("/batch/tags/preview")) {
        const { nodes } = JSON.parse(String(init?.body));
        return Response.json({
          tag_id: catalog[0].id,
          tag_revision: 2,
          nodes: nodes.map((node: object) => ({ ...node, assigned: applied })),
        });
      }
      const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
      if (id === 7) throw new TypeError("Network error");
      return Response.json({ id, name: `Document ${id}`, revision: 12 });
    });
    render(SnapshotActions, props({ scope: "selection", count: 2, targets }));
    await chooseReview();
    await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Check tag results" }));
    await fireEvent.click(
      await screen.findByRole("button", { name: "Add “Review” to 1 document" }),
    );
    await waitFor(() => expect(writes).toHaveLength(2));
    if (outcome === "rejected") {
      await screen.findByText("Retry refused");
      expect(screen.getByRole("button", { name: "Check tag results" })).toBeTruthy();
    } else {
      if (outcome === "stale")
        await screen.findByText("Some documents changed. Check their tags again.");
      await waitFor(() =>
        expect(screen.queryByRole("button", { name: "Add “Review” to 1 document" })).toBeNull(),
      );
      await fireEvent.click(
        await screen.findByRole("button", {
          name: "Check tag results",
        }),
      );
      await screen.findByText(
        outcome === "unknown" ? /1 matched · 0 remaining/ : /0 matched · 1 remaining/,
      );
    }
  },
);

it("pages checked documents and resets to the first page on another check", async () => {
  const many = Array.from({ length: 51 }, (_, i) => ({
    node_id: i + 1,
    revision: 1,
    name: `File ${i + 1}`,
  }));
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/batch/tags") throw new TypeError("Lost response");
    if (url.endsWith(`/tags/${catalog[0].id}`)) return Response.json(catalog[0]);
    if (url.endsWith("/batch/tags/preview")) {
      const { nodes } = JSON.parse(String(init?.body));
      return Response.json({
        tag_id: catalog[0].id,
        tag_revision: 2,
        nodes: nodes.map((node: object) => ({ ...node, assigned: false })),
      });
    }
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

it.each([true, false])(
  "offers a fresh check after a confirmed retry (assign=%s)",
  async (assign) => {
    const writes: BatchTagRequest[] = [];
    const selected = [1, 2, 3].map((node_id) => ({
      node_id,
      revision: 1,
      name: `File ${node_id}`,
    }));
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === "/api/v1/batch/tags") {
        const request = JSON.parse(String(init?.body)) as BatchTagRequest;
        writes.push(request);
        if (writes.length === 1) throw new TypeError("Lost response");
        return Response.json(await receiptFor(request));
      }
      if (url.endsWith(`/tags/${catalog[0].id}`)) return Response.json(catalog[0]);
      if (url.endsWith("/batch/tags/preview")) {
        const { nodes } = JSON.parse(String(init?.body));
        const matched = writes.length === 1 ? nodes[0].node_id === 1 : nodes[0].node_id === 2;
        return Response.json({
          tag_id: catalog[0].id,
          tag_revision: 2,
          nodes: [{ ...nodes[0], assigned: matched === assign }],
        });
      }
      const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
      if (id === 3) throw new TypeError("unavailable");
      return Response.json({
        id,
        name: `Document ${id}`,
        path: `/Document ${id}`,
        revision: 10 + id,
      });
    });
    render(SnapshotActions, props({ scope: "selection", count: 3, targets: selected }));
    await chooseReview();
    if (!assign) await fireEvent.click(screen.getByRole("radio", { name: "Remove" }));
    await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Check tag results" }));
    await fireEvent.click(
      await screen.findByRole("button", {
        name: assign ? "Add “Review” to 1 document" : "Remove “Review” from 1 document",
      }),
    );
    await screen.findByText(
      assign ? "Added “Review” to 1 document." : "Removed “Review” from 1 document.",
    );
    await fireEvent.click(screen.getByRole("button", { name: "Check tag results" }));
    await screen.findByText("1 matched · 1 remaining · 1 couldn't be checked");
    expect(writes[1].nodes).toEqual([{ node_id: 2, revision: 12 }]);
    expect(writes[1].operation_id).not.toBe(writes[0].operation_id);
    await fireEvent.click(screen.getByText("Review documents"));
    expect(screen.getByText("/Document 1").parentElement?.textContent).toContain(
      assign ? "Doesn't have “Review”" : "Has “Review”",
    );
    expect(screen.getByText("File 3").parentElement?.textContent).toContain("Couldn't check");
  },
);

it.each([false, true])("preserves a deleted tag error (after check: %s)", async (afterCheck) => {
  let writes = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/batch/tags") {
      writes++;
      if (writes === 1) throw new TypeError("Lost response");
      return Response.json({ code: "not_found", detail: "Tag was deleted" }, { status: 404 });
    }
    if (url.endsWith(`/tags/${catalog[0].id}`)) {
      if (!afterCheck || writes > 1)
        return Response.json({ code: "not_found", detail: "Tag was deleted" }, { status: 404 });
      return Response.json(catalog[0]);
    }
    if (url.endsWith("/batch/tags/preview")) {
      const { nodes } = JSON.parse(String(init?.body));
      return Response.json({
        tag_id: catalog[0].id,
        tag_revision: 2,
        nodes: nodes.map((node: object) => ({ ...node, assigned: false })),
      });
    }
    const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
    return Response.json({ id, name: `Document ${id}`, revision: 5 });
  });
  render(SnapshotActions, props({ scope: "selection", count: 2, targets }));
  await chooseReview();
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Check tag results" }));
  if (afterCheck) {
    await fireEvent.click(
      await screen.findByRole("button", { name: "Add “Review” to 2 documents" }),
    );
    await screen.findByText("Some documents changed. Check their tags again.");
    await fireEvent.click(screen.getByRole("button", { name: "Check tag results" }));
  }
  await screen.findByText("Tag was deleted");
  expect(screen.queryByRole("button", { name: /Add “Review”/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /Check/ })).toBeNull();
  expect(writes).toBe(afterCheck ? 2 : 1);
});

it("keeps the confirmed retry visible when a later check fails", async () => {
  let writes = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input) === "/api/v1/batch/tags") {
      if (++writes === 1) throw new TypeError("Lost response");
      return Response.json(await receiptFor(JSON.parse(String(init?.body))));
    }
    if (String(input).endsWith(`/tags/${catalog[0].id}`)) {
      if (writes > 1) throw new TypeError("Check failed");
      return Response.json(catalog[0]);
    }
    if (String(input).endsWith("/batch/tags/preview")) {
      const { nodes } = JSON.parse(String(init?.body));
      return Response.json({
        tag_id: catalog[0].id,
        tag_revision: 2,
        nodes: nodes.map((node: object) => ({ ...node, assigned: false })),
      });
    }
    return Response.json({ id: 7, name: "File 7", revision: 5 });
  });
  render(SnapshotActions, props({ scope: "selection", count: 1, targets: [targets[0]] }));
  await chooseReview();
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Check tag results" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Add “Review” to 1 document" }));
  await screen.findByText("Added “Review” to 1 document.");
  await fireEvent.click(screen.getByRole("button", { name: "Check tag results" }));
  await screen.findByText("Check failed");
  expect(screen.getByText("Added “Review” to 1 document.")).toBeTruthy();
});

it("keeps recovery controls usable while the surrounding view loads", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/batch/tags") throw new TypeError("Lost response");
    if (url.endsWith(`/tags/${catalog[0].id}`)) return Response.json(catalog[0]);
    if (url.endsWith("/batch/tags/preview")) {
      const { nodes } = JSON.parse(String(init?.body));
      return Response.json({
        tag_id: catalog[0].id,
        tag_revision: 2,
        nodes: [{ ...nodes[0], assigned: false }],
      });
    }
    return Response.json({ id: 7, name: "File 7", revision: 5 });
  });
  const view = render(
    SnapshotActions,
    props({ scope: "selection", count: 1, targets: [targets[0]] }),
  );
  await chooseReview();
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await screen.findByRole("button", { name: "Check tag results" });
  await view.rerender(
    props({ scope: "selection", count: 1, targets: [targets[0]], disabled: true }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Check tag results" }));
  const retry = await screen.findByRole("button", { name: "Add “Review” to 1 document" });
  expect((retry as HTMLButtonElement).disabled).toBe(false);
  expect((screen.getByRole("button", { name: "Check again" }) as HTMLButtonElement).disabled).toBe(
    false,
  );
});
