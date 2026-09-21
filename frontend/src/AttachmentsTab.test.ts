import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import AttachmentsTab from "./AttachmentsTab.svelte";
import type { SelectedSource } from "./selectedSource.js";

const version = "11111111-1111-4111-8111-111111111111";
const parent = { node_id: 1, version_id: version, sha256: "a".repeat(64), size: 12 };
const child = { node_id: 2, version_id: "22222222-2222-4222-8222-222222222222", sha256: "b".repeat(64), size: 4 };
const source: SelectedSource = { kind: "live", key: "root", nodeID: 1, versionID: version, blobHash: parent.sha256, size: 12, mutationRevision: 1, name: "parent.eml", path: "/parent.eml", mimeType: "message/rfc822", modifiedAt: "2026-09-12T00:00:00Z" };
const relation = (order: number) => ({ operation_id: "synthetic", order, parent, generation_id: "c".repeat(64), attachment_id: "d".repeat(64), part_path: `1.${order + 1}`, sibling_order: order, filename: "same.txt", outcome: "decoded", child });
const empty = { items: [], total: 0, next_operation_id: "", next_order: 0 };
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it("loads one explicit page and checks partial inventory without collapsing equal occurrences", async () => {
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const route = String(input); calls.push(route);
    if (route.includes("child_version_id=")) return Response.json(empty);
    if (route.includes("email-document-publications")) return Response.json({ operation_id: "synthetic", request_digest: "e".repeat(64), created_at: "2026-09-12T00:00:00Z", inventory_state: "partial", relations: Array.from({ length: 51 }, (_, i) => relation(i + 1)) });
    const more = route.includes("after_order=");
    return Response.json({ items: Array.from({ length: more ? 1 : 50 }, (_, i) => ({ relation: relation(more ? 51 : i + 1), state: "pending", reason: "text_extraction" })), total: 51, next_operation_id: more ? "" : "synthetic", next_order: more ? 0 : 50 });
  });
  const onopen = vi.fn();
  render(AttachmentsTab, { session: "session", source, onopen, onauthfailure: vi.fn() });
  const outgoing = screen.getByRole("region", { name: "Outgoing attachments" });
  await waitFor(() => expect(within(outgoing).getAllByRole("listitem")).toHaveLength(50));
  expect(calls).toHaveLength(2);
  expect(screen.getByText(/No published parent occurrences/)).toBeTruthy();
  await fireEvent.click(within(outgoing).getByRole("button", { name: "Load more attachments" }));
  await waitFor(() => expect(within(outgoing).getAllByRole("listitem")).toHaveLength(51));
  await fireEvent.click(within(outgoing).getByRole("button", { name: "Check inventory for synthetic" }));
  expect(await screen.findByText("Publication inventory: partial")).toBeTruthy();
  expect(within(outgoing).getAllByText(/Processing: pending/)).toHaveLength(51);
  await fireEvent.click(within(outgoing).getByRole("button", { name: "Open attachment same.txt, part 1.52" }));
  expect(onopen).toHaveBeenCalledWith(child);
});

it("keeps unavailable occurrences visible and fences replaced-source and denied reads", async () => {
  let release!: (response: Response) => void;
  let oldSignal: AbortSignal | null | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const route = String(input);
    if (route.includes(version) && route.includes("parent_version_id")) {
      oldSignal = init?.signal;
      return new Promise<Response>((resolve) => { release = resolve; });
    }
    if (route.includes("child_version_id")) return Response.json({ detail: "Not allowed" }, { status: 403 });
    return Response.json({ items: [{ relation: { ...relation(1), parent: { ...parent, version_id: child.version_id }, outcome: "encrypted", child: null }, state: "encrypted", reason: "mime_encrypted" }], total: 1, next_operation_id: "", next_order: 0 });
  });
  const view = render(AttachmentsTab, { session: "session", source, onopen: vi.fn(), onauthfailure: vi.fn() });
  await waitFor(() => expect(release).toBeTypeOf("function"));
  await view.rerender({ session: "session", source: { ...source, versionID: child.version_id, key: "new-root" }, onopen: vi.fn(), onauthfailure: vi.fn() });
  expect(await screen.findByText(/MIME: encrypted/)).toBeTruthy();
  expect(oldSignal?.aborted).toBe(true);
  release(Response.json(empty));
  expect(await screen.findByText(/Parent relations unavailable: Not allowed/)).toBeTruthy();
  expect(screen.getByText(/MIME: encrypted/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Open attachment/ })).toBeNull();
});

it("discards a delayed publication receipt after changing the selected source", async () => {
  let release!: (response: Response) => void;
  let signal: AbortSignal | null | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const route = String(input);
    if (route.includes("email-document-publications")) {
      signal = init?.signal;
      return new Promise<Response>((resolve) => { release = resolve; });
    }
    return Response.json(route.includes(`parent_version_id=${version}`)
      ? { ...empty, items: [{ relation: relation(1), state: "decoded", reason: "processing_not_requested" }], total: 1 }
      : empty);
  });
  const view = render(AttachmentsTab, { session: "session", source, onopen: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Check inventory for synthetic" }));
  await waitFor(() => expect(release).toBeTypeOf("function"));
  await view.rerender({ session: "session", source: { ...source, key: "new-root", versionID: child.version_id }, onopen: vi.fn(), onauthfailure: vi.fn() });
  expect(signal?.aborted).toBe(true);
  release(Response.json({ operation_id: "synthetic", request_digest: "e".repeat(64), created_at: "2026-09-12T00:00:00Z", inventory_state: "complete", relations: [relation(1)] }));
  await waitFor(() => expect(screen.getByText(/No published attachment occurrences/)).toBeTruthy());
  expect(screen.queryByText("Publication inventory: complete")).toBeNull();
});
