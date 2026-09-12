import { webcrypto } from "node:crypto";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import BatchTagsModal from "./BatchTagsModal.svelte";

const tag = { id: "22222222-2222-4222-8222-222222222222", name: "Review", revision: 2, assignment_count: 1 };
const targets = [{ node_id: 7, revision: 3 }, { node_id: 9, revision: 4 }];

beforeEach(() => {
  vi.stubGlobal("crypto", webcrypto);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); });

async function chooseTag() {
  await fireEvent.click(screen.getByRole("combobox", { name: "Tag for selected documents: Choose a tag…" }));
  await fireEvent.click(screen.getByRole("option", { name: "Review" }));
}

it("shows exact mixed membership and retries the same uncertain operation", async () => {
  const requests: { operation_id: string; nodes: typeof targets }[] = [];
  const onchanged = vi.fn();
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    if (String(url).endsWith("/preview")) {
      const complete = requests.length === 2;
      return new Response(JSON.stringify({ tag_id: tag.id, tag_revision: complete ? 3 : 2,
        nodes: [{ node_id: 7, revision: 3, assigned: true }, { node_id: 9, revision: complete ? 5 : 4, assigned: complete }],
      }), { status: 200 });
    }
    const request = JSON.parse(String(init?.body));
    requests.push(request);
    if (requests.length === 1) throw new TypeError("Synthetic response loss");
    return new Response(JSON.stringify({ version: 1, operation_id: request.operation_id,
      request_digest: "9d29429ce1a103db51351b4d136306a00fb8dc77ae392b506ba09e625073c725",
      tag_id: tag.id, assign: true, tag_revision: 3, assignment_count: 2,
      completed_at: "2026-09-11T00:00:00.000000000Z",
      nodes: [{ node_id: 7, expected_revision: 3, revision: 3, changed: false }, { node_id: 9, expected_revision: 4, revision: 5, changed: true }],
    }), { status: 200 });
  });
  render(BatchTagsModal, { session: "session", targets, catalog: [tag], catalogTotal: 1,
    disabled: false, onclose: vi.fn(), onchanged, onauthfailure: vi.fn() });
  await chooseTag();
  await screen.findByText("1 of 2 selected documents have this tag.");
  expect((screen.getByRole("checkbox", { name: "Selected tag membership" }) as HTMLInputElement).indeterminate).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Add to all" }));
  await screen.findByRole("button", { name: "Retry same operation" });
  expect(onchanged).not.toHaveBeenCalled();
  expect((screen.getByRole("button", { name: "Remove from all" }) as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Retry same operation" }));
  await waitFor(() => expect(onchanged).toHaveBeenCalledTimes(1));
  expect(requests).toHaveLength(2);
  expect(requests[1]).toEqual(requests[0]);
  await screen.findByText("2 of 2 selected documents have this tag.");
});

it("requires explicit reselection after a stale preview", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ detail: "Selection is stale", code: "stale_revision" }), { status: 412 }));
  const onclose = vi.fn();
  render(BatchTagsModal, { session: "session", targets, catalog: [tag], catalogTotal: 3,
    disabled: false, onclose, onchanged: vi.fn(), onauthfailure: vi.fn() });
  await chooseTag();
  await screen.findByText(/Close and refresh your selection/);
  expect((screen.getByRole("button", { name: "Add to all" }) as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Done" }));
  expect(onclose).toHaveBeenCalledOnce();
});

it.each([
  { status: 412, code: "stale_revision", refresh: true },
  { status: 404, code: "not_found", refresh: true },
  { status: 409, code: "audit_mutation_unsupported", refresh: false },
  { status: 409, code: "batch_tag_operation_conflict", refresh: false },
])("handles $code after a successful preview", async ({ status, code, refresh }) => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url) => {
    if (String(url).endsWith("/preview")) {
      return new Response(JSON.stringify({ tag_id: tag.id, tag_revision: 2,
        nodes: targets.map((target, index) => ({ ...target, assigned: index === 0 })),
      }));
    }
    return new Response(JSON.stringify({ detail: code, code }), { status });
  });
  render(BatchTagsModal, { session: "session", targets, catalog: [tag], catalogTotal: 1,
    disabled: false, onclose: vi.fn(), onchanged: vi.fn(), onauthfailure: vi.fn() });
  await chooseTag();
  await screen.findByText("1 of 2 selected documents have this tag.");
  await fireEvent.click(screen.getByRole("button", { name: "Add to all" }));
  expect((await screen.findByRole("alert")).textContent).toBe(code);
  expect(screen.queryByText(/Close and refresh your selection/) !== null).toBe(refresh);
  expect((screen.getByRole("button", { name: "Add to all" }) as HTMLButtonElement).disabled).toBe(refresh);
  expect((screen.getByRole("button", { name: "Remove from all" }) as HTMLButtonElement).disabled).toBe(refresh);
  expect(screen.queryByRole("button", { name: "Retry same operation" })).toBeNull();
});
