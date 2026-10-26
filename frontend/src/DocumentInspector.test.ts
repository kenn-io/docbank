import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
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

it.each([
  { label: "Extract text", rendition: true, embedding_bindings: [], query_embedding_bindings: [] },
  {
    label: "Index for search",
    rendition: false,
    embedding_bindings: ["text"],
    query_embedding_bindings: ["text"],
  },
])(
  "offers $label when a matching processing profile is configured",
  async ({ label, ...capabilities }) => {
    const onprocess = vi.fn();
    const props = { ...fileProps, onprocess };
    const view = render(DocumentInspector, props);
    expect(screen.queryByRole("button", { name: label })).toBeNull();
    await view.rerender({
      ...props,
      profiles: [{ name: "Processing", fingerprint: "a".repeat(64), ...capabilities }],
    });
    await fireEvent.click(await screen.findByRole("button", { name: label }));
    expect(onprocess).toHaveBeenCalledOnce();
  },
);

it("opens folder protection from its actions and copies the command", async () => {
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
