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
  expect(screen.queryByText("No published attachments")).toBeNull();
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
  expect(
    await screen.findByText(/Couldn't load the document this file is attached to: Not allowed/),
  ).toBeTruthy();
  expect(screen.getByText("encrypted")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "same.txt" })).toBeNull();
  expect(screen.queryByText("No published attachments")).toBeNull();
});

it("keeps the first page while retrying a failed next page", async () => {
  let laterCalls = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const route = String(input);
    if (route.includes("child_version_id=")) return Response.json(empty);
    if (route.includes("after_order=")) {
      laterCalls++;
      if (laterCalls <= 2) return Response.json({ detail: "Unavailable" }, { status: 503 });
      return Response.json({
        items: [{ relation: relation(51), state: "pending", reason: "text_extraction" }],
        total: 51,
        next_operation_id: "",
        next_order: 0,
      });
    }
    return Response.json({
      items: Array.from({ length: 50 }, (_, i) => ({
        relation: relation(i + 1),
        state: "pending",
        reason: "text_extraction",
      })),
      total: 51,
      next_operation_id: "synthetic",
      next_order: 50,
    });
  });
  render(AttachmentsTab, { session: "session", source, onopen: vi.fn(), onauthfailure: vi.fn() });
  const group = await screen.findByRole("region", { name: "Attachments" });
  await fireEvent.click(await within(group).findByRole("button", { name: "Show more" }));
  const retry = await within(group).findByRole("button", { name: "Try again" });
  expect(within(group).getAllByRole("listitem")).toHaveLength(50);
  await fireEvent.click(retry);
  await waitFor(() => expect(within(group).getAllByRole("listitem")).toHaveLength(51));
  expect(laterCalls).toBe(3);
});

it.each([401, 403, 404, 400, 0])(
  "retains rows after a permanent pagination failure %s",
  async (status) => {
    let laterCalls = 0;
    const onauthfailure = vi.fn();
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const route = String(input);
      if (route.includes("child_version_id=")) return Response.json(empty);
      if (route.includes("after_order=")) {
        laterCalls++;
        if (status === 0) return Response.json({ invalid: true });
        return Response.json({ detail: "Unavailable" }, { status });
      }
      return Response.json({
        items: Array.from({ length: 50 }, (_, i) => ({
          relation: relation(i + 1),
          state: "pending",
          reason: "text_extraction",
        })),
        total: 51,
        next_operation_id: "synthetic",
        next_order: 50,
      });
    });
    render(AttachmentsTab, { session: "session", source, onopen: vi.fn(), onauthfailure });
    const group = await screen.findByRole("region", { name: "Attachments" });
    await fireEvent.click(await within(group).findByRole("button", { name: "Show more" }));
    expect(
      await within(group).findByText(status === 0 ? /Invalid attachment response/ : /Unavailable/),
    ).toBeTruthy();
    expect(laterCalls).toBe(1);
    expect(onauthfailure).toHaveBeenCalledTimes(status === 401 ? 1 : 0);
    expect(within(group).getAllByRole("listitem")).toHaveLength(50);
    expect(within(group).queryByRole("button", { name: "Show more" })).toBeNull();
    expect(within(group).queryByRole("button", { name: "Try again" })).toBeNull();
  },
);

it("does not retry invalid UTF-8 in an attachment response", async () => {
  let outgoing = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (String(input).includes("child_version_id=")) return Response.json(empty);
    outgoing++;
    return new Response(new Uint8Array([0xff]));
  });
  render(AttachmentsTab, { session: "session", source, onopen: vi.fn(), onauthfailure: vi.fn() });
  const group = await screen.findByRole("region", { name: "Attachments" });
  expect(await within(group).findByText(/invalid UTF-8/)).toBeTruthy();
  expect(within(group).queryByRole("button", { name: "Try again" })).toBeNull();
  expect(outgoing).toBe(1);
});

it("retains rows when relations change between pages", async () => {
  let firstCalls = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const route = String(input);
    if (route.includes("child_version_id=")) return Response.json(empty);
    if (route.includes("after_order="))
      return Response.json({
        items: [{ relation: relation(51), state: "pending", reason: "text_extraction" }],
        total: 52,
        next_operation_id: "",
        next_order: 0,
      });
    firstCalls++;
    return Response.json({
      items: Array.from({ length: 50 }, (_, i) => ({
        relation: relation(i + 1),
        state: "pending",
        reason: "text_extraction",
      })),
      total: 51,
      next_operation_id: "synthetic",
      next_order: 50,
    });
  });
  render(AttachmentsTab, { session: "session", source, onopen: vi.fn(), onauthfailure: vi.fn() });
  const group = await screen.findByRole("region", { name: "Attachments" });
  await fireEvent.click(await within(group).findByRole("button", { name: "Show more" }));
  expect(await within(group).findByText(/relations changed/i)).toBeTruthy();
  expect(within(group).getAllByRole("listitem")).toHaveLength(50);
  expect(within(group).queryByRole("button", { name: "Show more" })).toBeNull();
  expect(within(group).queryByRole("button", { name: "Try again" })).toBeNull();
  expect(firstCalls).toBe(1);
});

it("automatically retries a refreshed source after a manual retry", async () => {
  let calls = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (String(input).includes("child_version_id=")) return Response.json(empty);
    calls++;
    if (calls === 1 || calls === 2 || calls === 4)
      return Response.json({ detail: "Unavailable" }, { status: 503 });
    return Response.json(empty);
  });
  const props = { session: "session", source, onopen: vi.fn(), onauthfailure: vi.fn() };
  const view = render(AttachmentsTab, props);
  await fireEvent.click(await screen.findByRole("button", { name: "Try again" }));
  await screen.findByText("No published attachments");
  await view.rerender({ ...props, source: { ...source } });
  await waitFor(() => expect(calls).toBe(5));
  expect(await screen.findByText("No published attachments")).toBeTruthy();
});

it("retries an interrupted attachment response stream", async () => {
  let outgoing = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (String(input).includes("child_version_id=")) return Response.json(empty);
    outgoing++;
    if (outgoing === 1)
      return new Response(
        new ReadableStream({
          start(controller) {
            controller.error(new TypeError("Connection lost"));
          },
        }),
      );
    return Response.json(empty);
  });
  render(AttachmentsTab, { session: "session", source, onopen: vi.fn(), onauthfailure: vi.fn() });
  expect(await screen.findByText("No published attachments")).toBeTruthy();
  expect(outgoing).toBe(2);
});
