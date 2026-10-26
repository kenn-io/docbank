import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import AttachmentsTab from "./AttachmentsTab.svelte";
import type { SelectedSource } from "./selectedSource.js";

const version = "11111111-1111-4111-8111-111111111111";
const parent = { node_id: 1, version_id: version, sha256: "a".repeat(64), size: 12 };
const child = {
  node_id: 2,
  version_id: "22222222-2222-4222-8222-222222222222",
  sha256: "b".repeat(64),
  size: 4,
};
const source: SelectedSource = {
  kind: "live",
  key: "root",
  nodeID: 1,
  versionID: version,
  blobHash: parent.sha256,
  size: 12,
  mutationRevision: 1,
  name: "parent.eml",
  path: "/parent.eml",
  mimeType: "message/rfc822",
  modifiedAt: "2026-09-12T00:00:00Z",
};
const relation = (order: number) => ({
  operation_id: "synthetic",
  order,
  parent,
  generation_id: "c".repeat(64),
  attachment_id: "d".repeat(64),
  part_path: `1.${order + 1}`,
  sibling_order: order,
  filename: "same.txt",
  outcome: "decoded",
  child,
});
const empty = { items: [], total: 0, next_operation_id: "", next_order: 0 };
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it("lists one page of attachments and opens a row", async () => {
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const route = String(input);
    calls.push(route);
    if (route.includes("child_version_id=")) return Response.json(empty);
    const more = route.includes("after_order=");
    return Response.json({
      items: Array.from({ length: more ? 1 : 50 }, (_, i) => ({
        relation: relation(more ? 51 : i + 1),
        state: "pending",
        reason: "text_extraction",
      })),
      total: 51,
      next_operation_id: more ? "" : "synthetic",
      next_order: more ? 0 : 50,
    });
  });
  const onopen = vi.fn();
  render(AttachmentsTab, { session: "session", source, onopen, onauthfailure: vi.fn() });
  const outgoing = await screen.findByRole("region", { name: "Attachments" });
  await waitFor(() => expect(within(outgoing).getAllByRole("listitem")).toHaveLength(50));
  expect(calls).toHaveLength(2);
  expect(screen.queryByRole("region", { name: "Attached to" })).toBeNull();
  expect(screen.queryByText("No attachments")).toBeNull();
  await fireEvent.click(within(outgoing).getByRole("button", { name: "Show more" }));
  await waitFor(() => expect(within(outgoing).getAllByRole("listitem")).toHaveLength(51));
  expect(within(outgoing).queryByRole("button", { name: "Show more" })).toBeNull();
  await fireEvent.click(within(outgoing).getAllByRole("button", { name: "same.txt" })[0]);
  expect(onopen).toHaveBeenCalledWith(child);
});

it("qualifies an empty relation list as no published attachments", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => Response.json(empty));
  render(AttachmentsTab, {
    session: "session",
    source,
    onopen: vi.fn(),
    onauthfailure: vi.fn(),
  });
  expect(await screen.findByText("No published attachments")).toBeTruthy();
  expect(screen.queryByRole("region")).toBeNull();
});

it("lists the documents an attachment belongs to by name", async () => {
  const mail = { node_id: 3, version_id: version, sha256: "a".repeat(64), size: 12 };
  const attached = { ...child, node_id: 2, version_id: child.version_id };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const route = String(input);
    if (route.includes("/api/v1/nodes/3")) return Response.json({ id: 3, name: "mail.eml" });
    if (route.includes("child_version_id="))
      return Response.json({
        items: [
          {
            relation: { ...relation(1), parent: mail, child: attached },
            state: "decoded",
            reason: "processing_not_requested",
          },
        ],
        total: 1,
        next_operation_id: "",
        next_order: 0,
      });
    return Response.json(empty);
  });
  const onopen = vi.fn();
  render(AttachmentsTab, {
    session: "session",
    source: { ...source, nodeID: 2, versionID: child.version_id, blobHash: child.sha256, size: 4 },
    onopen,
    onauthfailure: vi.fn(),
  });
  const group = await screen.findByRole("region", { name: "Attached to" });
  await fireEvent.click(await within(group).findByRole("button", { name: "mail.eml" }));
  expect(onopen).toHaveBeenCalledWith(mail);
  expect(screen.queryByRole("region", { name: "Attachments" })).toBeNull();
});

it("keeps unavailable occurrences visible and fences replaced-source and denied reads", async () => {
  let release!: (response: Response) => void;
  let oldSignal: AbortSignal | null | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const route = String(input);
    if (route.includes(version) && route.includes("parent_version_id")) {
      oldSignal = init?.signal;
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    }
    if (route.includes("child_version_id"))
      return Response.json({ detail: "Not allowed" }, { status: 403 });
    return Response.json({
      items: [
        {
          relation: {
            ...relation(1),
            parent: { ...parent, version_id: child.version_id },
            outcome: "encrypted",
            child: null,
          },
          state: "encrypted",
          reason: "mime_encrypted",
        },
      ],
      total: 1,
      next_operation_id: "",
      next_order: 0,
    });
  });
  const view = render(AttachmentsTab, {
    session: "session",
    source,
    onopen: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await waitFor(() => expect(release).toBeTypeOf("function"));
  await view.rerender({
    session: "session",
    source: { ...source, versionID: child.version_id, key: "new-root" },
    onopen: vi.fn(),
    onauthfailure: vi.fn(),
  });
  expect(await screen.findByText("encrypted")).toBeTruthy();
  expect(oldSignal?.aborted).toBe(true);
  release(Response.json(empty));
  expect(await screen.findByText(/Parent relations unavailable: Not allowed/)).toBeTruthy();
  expect(screen.getByText("encrypted")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "same.txt" })).toBeNull();
  expect(screen.queryByText("No attachments")).toBeNull();
});
