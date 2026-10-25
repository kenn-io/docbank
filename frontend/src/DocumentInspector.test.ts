import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import * as api from "./generated/docbank.js";
import DocumentInspector from "./DocumentInspector.svelte";
import type { Node } from "./generated/docbank.js";

const folder: Node = {
  id: 42,
  name: "Records",
  kind: "dir",
  revision: 1,
  size: 0,
  created_at: "2026-01-01T00:00:00Z",
  modified_at: "2026-01-01T00:00:00Z",
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("offers text extraction only after a configured rendition profile accepts the selected version", async () => {
  const eligiblePlan: api.ProcessingPlan = {
    selector: { node_id: 42, content_version_id: "version-1", profile: "Text" },
    fingerprint: "a".repeat(64),
    profile_fingerprint: "b".repeat(64),
    vault_uid: "test-vault",
    backup_consequence: "",
    consent_required: false,
    consent_state: "active",
    disclosed_classes: [],
    retained_classes: [],
    flow: [],
    estimate: { provider_calls: 1, source_bytes: 64, vector_spaces: 0 },
  };
  const plan = vi
    .spyOn(api, "planDocumentProcessing")
    .mockResolvedValue({ ...eligiblePlan, rendition_ineligible_reason: "Unsupported media type" });
  const onprocess = vi.fn();
  const file = {
    ...folder,
    kind: "file" as const,
    name: "report.txt",
    mime_type: "text/plain",
    current_version_id: "version-1",
    size: 64,
  };
  const profile = {
    name: "Text",
    rendition: true,
    fingerprint: "a".repeat(64),
    embedding_bindings: [],
    query_embedding_bindings: [],
  };
  const props = {
    node: file,
    path: "/report.txt",
    session: "test-session",
    tags: [],
    tagsLoading: false,
    tagsError: "",
    onprovenance: vi.fn(),
    onhistory: vi.fn(),
    onauthfailure: vi.fn(),
    onprocess,
  };
  const view = render(DocumentInspector, props);
  expect(screen.getByText("Plain text · 64 B")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Extract text" })).toBeNull();
  expect(plan).not.toHaveBeenCalled();
  await view.rerender({ ...props, profiles: [profile] });
  await waitFor(() => expect(plan).toHaveBeenCalledOnce());
  expect(screen.queryByRole("button", { name: "Extract text" })).toBeNull();
  plan.mockResolvedValue(eligiblePlan);
  await view.rerender({
    ...props,
    profiles: [profile],
    node: { ...file, current_version_id: "version-2" },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Extract text" }));
  expect(plan).toHaveBeenLastCalledWith(
    { selector: { node_id: 42, content_version_id: "version-2", profile: "Text" } },
    { session: "test-session", signal: expect.any(AbortSignal) },
  );
  expect(onprocess).toHaveBeenCalledOnce();
});

it("opens the folder protection command from its actions and copies the exact folder ID", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  const onopen = vi.fn();
  const ontrash = vi.fn();
  render(DocumentInspector, {
    node: folder,
    path: "/Records",
    tags: [],
    tagsLoading: false,
    tagsError: "",
    onopen,
    ontrash,
    onprovenance: vi.fn(),
    onhistory: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Open" }));
  expect(onopen).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: "Document actions" }));
  const menu = await screen.findByRole("menu");
  expect(menu.querySelectorAll('[role="menuitem"]')[0]?.textContent).toContain(
    "Protect permanently…",
  );
  expect(menu.querySelectorAll('[role="menuitem"]')[1]?.textContent).toContain("Move to trash");
  await fireEvent.click(screen.getByRole("menuitem", { name: "Protect permanently…" }));
  expect(await screen.findByRole("dialog", { name: "Protect permanently" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Copy protection command" }));
  expect(writeText).toHaveBeenCalledWith("docbank audit enable --node-id 42");
});

it("opens audit history from the protected chip and tag management from an empty Tags row", async () => {
  const onhistory = vi.fn();
  const ontags = vi.fn();
  render(DocumentInspector, {
    node: folder,
    path: "/Records",
    protected: true,
    tags: [],
    tagsLoading: false,
    tagsError: "",
    onprovenance: vi.fn(),
    onhistory,
    ontags,
  });
  await fireEvent.click(screen.getByRole("button", { name: "Protected, open Audit history" }));
  expect(onhistory).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: "Add tag" }));
  expect(ontags).toHaveBeenCalledOnce();
});

const fileProps = {
  node: {
    ...folder,
    kind: "file" as const,
    name: "notes.txt",
    mime_type: "text/plain; charset=utf-8",
    current_version_id: "11111111-1111-4111-8111-111111111111",
    blob_hash: "c".repeat(64),
    size: 12,
  },
  path: "/notes.txt",
  tags: [],
  tagsLoading: false,
  tagsError: "",
  onprovenance: vi.fn(),
  onhistory: vi.fn(),
};

it("trashes a file from one icon button and keeps identifiers behind a disclosure", async () => {
  const ontrash = vi.fn();
  render(DocumentInspector, { ...fileProps, ontrash });

  expect(screen.queryByRole("button", { name: "Document actions" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
  expect(ontrash).toHaveBeenCalledOnce();

  const disclosure = screen.getByText("Details").closest("details")!;
  expect(disclosure.open).toBe(false);
  expect(disclosure.textContent).toContain("text/plain");
  expect(disclosure.textContent).not.toContain("charset");
  expect(disclosure.textContent).toContain("c".repeat(64).slice(0, 8));
  expect(screen.getByText("Path").closest("details")).toBeNull();
  expect(screen.getByText("Size").closest("details")).toBeNull();
});

it("disables trash until the view is refreshed", () => {
  render(DocumentInspector, { ...fileProps, ontrash: vi.fn(), actionsDisabled: true });
  expect(
    (screen.getByRole("button", { name: "Move to trash" }) as HTMLButtonElement).disabled,
  ).toBe(true);
});
