import * as actions from "./action-events.js";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import VerifiedPreview from "./VerifiedPreview.svelte";
import type { SelectedSource } from "./selectedSource.js";

const identity = (id: number) => ({
  node_id: id,
  version_id: `${String(id).repeat(8)}-${String(id).repeat(4)}-4${String(id).repeat(3)}-8${String(id).repeat(3)}-${String(id).repeat(12)}`,
  sha256: String(id).repeat(64),
  size: 12,
});
const root = identity(1),
  child = identity(2),
  nested = identity(3);
const selected: SelectedSource = {
  kind: "snapshot",
  key: "frozen-root",
  nodeID: 1,
  versionID: root.version_id,
  blobHash: root.sha256,
  size: 12,
  mutationRevision: 1,
  name: "root.eml",
  path: "/root.eml",
  mimeType: "message/rfc822",
  modifiedAt: "2026-09-12T00:00:00Z",
  observedAt: "2026-09-12T00:00:00Z",
  originalTags: [],
  collectionIDs: [],
};
const relation = (parent: typeof root, child: typeof root) => ({
  operation_id: `publication-${parent.node_id}`,
  order: 1,
  parent,
  child,
  generation_id: "c".repeat(64),
  attachment_id: "d".repeat(64),
  part_path: "1.2",
  sibling_order: 1,
  filename: "nested.eml",
  outcome: "decoded",
});
const page = (entry?: ReturnType<typeof relation>) => ({
  items: entry ? [{ relation: entry, state: "decoded", reason: "processing_not_requested" }] : [],
  total: entry ? 1 : 0,
  next_operation_id: "",
  next_order: 0,
});
const version = (target: typeof root) => ({
  id: target.version_id,
  node_id: target.node_id,
  blob_hash: target.sha256,
  size: target.size,
  mime_type: "message/rfc822; charset=utf-8",
});
const node = (target: typeof root) => ({
  id: target.node_id,
  kind: "file",
  revision: 7,
  name: `message-${target.node_id}.eml`,
  path: `/message-${target.node_id}.eml`,
  mime_type: "text/plain",
  current_version_id: identity(9).version_id,
  modified_at: "2026-09-12T00:00:00Z",
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it("keeps nested exact contexts separate and returns to the frozen source and focus", async () => {
  const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
  const original = JSON.stringify(selected);
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const route = String(input);
    if (route === "/api/v1/renditions/text")
      return Response.json({
        state: "unprocessed",
        source: {
          node_id: child.node_id,
          revision: 7,
          version_id: child.version_id,
          blob_hash: child.sha256,
          size: child.size,
          media_type: "message/rfc822; charset=utf-8",
        },
        profile: { name: "", configuration: "configured", fingerprint: "b".repeat(64) },
        generation_id: "",
        attachment_id: "",
        build_id: "",
      });
    for (const target of [root, child, nested]) {
      if (route === `/api/v1/versions/${target.version_id}`) return Response.json(version(target));
      if (route === `/api/v1/nodes/${target.node_id}`) return Response.json(node(target));
      if (route.includes(`parent_version_id=${target.version_id}`))
        return Response.json(
          page(
            target.node_id < 3
              ? relation(target, target.node_id === 1 ? child : nested)
              : undefined,
          ),
        );
      if (route.includes(`child_version_id=${target.version_id}`))
        return Response.json(
          page(
            target.node_id > 1 ? relation(target.node_id === 2 ? root : child, target) : undefined,
          ),
        );
    }
    throw new Error(`Unexpected ${route}`);
  });
  const onreturnfocus = vi.fn();
  const ondisplay = vi.fn();
  const onprocess = vi.fn();
  render(VerifiedPreview, {
    session: "session",
    source: selected,
    authorizationRevision: 1,
    onreturnfocus,
    ondisplay,
    onprocess,
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("tab", { name: "Attachments" }));
  await fireEvent.click(await screen.findByRole("button", { name: "nested.eml" }));
  await waitFor(() =>
    expect(
      screen
        .getByRole("region", { name: "Verified content of message-2.eml" })
        .getAttribute("data-source-version"),
    ).toBe(child.version_id),
  );
  expect(ondisplay).toHaveBeenLastCalledWith(
    expect.objectContaining({ versionID: child.version_id, blobHash: child.sha256 }),
    7,
  );
  expect(screen.getByRole("button", { name: "Copy Version" }).textContent).toContain(
    child.version_id,
  );
  expect(screen.queryByRole("button", { name: "Download" })).toBeNull();
  expect(action).toHaveBeenCalledExactlyOnceWith("session", "document_opened");
  action.mockClear();
  await fireEvent.click(screen.getByRole("tab", { name: "Text" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Process…" }));
  expect(onprocess).toHaveBeenCalledWith(
    expect.objectContaining({ nodeID: child.node_id, versionID: child.version_id }),
  );
  await fireEvent.click(screen.getByRole("tab", { name: "Attachments" }));
  expect(await screen.findByRole("button", { name: "message-1.eml" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "nested.eml" }));
  await waitFor(() =>
    expect(
      screen
        .getByRole("region", { name: "Verified content of message-3.eml" })
        .getAttribute("data-source-version"),
    ).toBe(nested.version_id),
  );
  expect(action).toHaveBeenCalledExactlyOnceWith("session", "document_opened");
  action.mockClear();
  await fireEvent.click(screen.getByRole("button", { name: "Back to previous document" }));
  await waitFor(() =>
    expect(
      screen
        .getByRole("region", { name: "Verified content of message-2.eml" })
        .getAttribute("data-source-version"),
    ).toBe(child.version_id),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Return to selected document" }));
  await waitFor(() => expect(onreturnfocus).toHaveBeenCalledOnce());
  expect(screen.getByRole("region", { name: "Verified content of root.eml" })).toBeTruthy();
  expect(ondisplay).toHaveBeenLastCalledWith(selected, 1);
  expect(JSON.stringify(selected)).toBe(original);
  expect(action).not.toHaveBeenCalled();
});

it("cancels related version reads on source change and retains failed targets as visible occurrences", async () => {
  const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
  const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
  let release!: (value: Response) => void;
  let signal: AbortSignal | null | undefined;
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const route = String(input);
    if (route === `/api/v1/versions/${child.version_id}`) {
      signal = init?.signal;
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    }
    if (route.includes(`parent_version_id=${root.version_id}`))
      return Response.json(page(relation(root, child)));
    return Response.json(page());
  });
  const view = render(VerifiedPreview, {
    session: "session",
    source: selected,
    authorizationRevision: 1,
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("tab", { name: "Attachments" }));
  await fireEvent.click(await screen.findByRole("button", { name: "nested.eml" }));
  await waitFor(() => expect(release).toBeTypeOf("function"));
  await view.rerender({
    session: "replacement",
    source: {
      ...selected,
      key: "another",
      nodeID: 3,
      versionID: nested.version_id,
      blobHash: nested.sha256,
    },
    authorizationRevision: 1,
    onauthfailure: vi.fn(),
  });
  expect(signal?.aborted).toBe(true);
  release(Response.json(version(child)));
  await waitFor(() =>
    expect(screen.queryByText(`Exact related version ${child.version_id}`)).toBeNull(),
  );
  expect(fetch.mock.calls.some(([input]) => String(input) === "/api/v1/nodes/2")).toBe(false);
  // The read that a source change cancelled is not a failed open.
  expect(failure).not.toHaveBeenCalled();
  await view.rerender({
    session: "session",
    source: selected,
    authorizationRevision: 1,
    onauthfailure: vi.fn(),
  });
  fetch.mockImplementation(async (input) =>
    String(input).includes("/versions/")
      ? Response.json({ detail: "Related target missing" }, { status: 404 })
      : Response.json(
          String(input).includes("parent_version_id") ? page(relation(root, child)) : page(),
        ),
  );
  await fireEvent.click(await screen.findByRole("button", { name: "nested.eml" }));
  expect(
    await screen.findByText(/Related document unavailable: Related target missing/),
  ).toBeTruthy();
  expect(
    within(screen.getByRole("region", { name: "Attachments" })).getAllByRole("listitem"),
  ).toHaveLength(1);
  expect(action).not.toHaveBeenCalled();
  expect(failure).toHaveBeenCalledExactlyOnceWith(
    "session",
    "open",
    expect.objectContaining({ status: 404 }),
  );
});

it("reports one failed open when alternating documents reaches the navigation limit", async () => {
  const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
  const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const route = String(input);
    for (const target of [root, child]) {
      if (route === `/api/v1/versions/${target.version_id}`) return Response.json(version(target));
      if (route === `/api/v1/nodes/${target.node_id}`) return Response.json(node(target));
      if (route.includes(`parent_version_id=${target.version_id}`))
        return Response.json(page(target === root ? relation(root, child) : undefined));
      if (route.includes(`child_version_id=${target.version_id}`))
        return Response.json(page(target === child ? relation(root, child) : undefined));
    }
    throw new Error(`Unexpected ${route}`);
  });
  render(VerifiedPreview, {
    session: "session",
    source: selected,
    authorizationRevision: 1,
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("tab", { name: "Attachments" }));
  for (let index = 0; index < 32; index++) {
    const name = index % 2 === 0 ? "nested.eml" : "message-1.eml";
    await fireEvent.click(await screen.findByRole("button", { name }));
    await waitFor(() => expect(action).toHaveBeenCalledTimes(index + 1));
  }
  expect(failure).not.toHaveBeenCalled();
  await fireEvent.click(await screen.findByRole("button", { name: "nested.eml" }));
  await screen.findByText(/Navigation depth limit reached/);
  expect(failure).toHaveBeenCalledExactlyOnceWith("session", "open", expect.any(Error));
  expect(action).toHaveBeenCalledTimes(32);
});
