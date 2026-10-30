import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import DocumentInspector from "./DocumentInspector.svelte";
import type { ContentVersion, Node } from "./generated/docbank.js";

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

it("opens audit history from the protected chip", async () => {
  const onhistory = vi.fn();
  render(DocumentInspector, {
    node: folder,
    path: "/Records",
    protected: true,
    tags: [],
    tagsLoading: false,
    tagsError: "",
    onprovenance: vi.fn(),
    onhistory,
  });
  await fireEvent.click(screen.getByRole("button", { name: "Protected, open Audit history" }));
  expect(onhistory).toHaveBeenCalledOnce();
});
it("opens tag management from an empty Tags row", async () => {
  const ontags = vi.fn();
  render(DocumentInspector, {
    node: folder,
    path: "/Records",
    tags: [],
    tagsLoading: false,
    tagsError: "",
    onprovenance: vi.fn(),
    onhistory: vi.fn(),
    ontags,
  });
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

it("trashes a file from one icon button", async () => {
  const ontrash = vi.fn();
  render(DocumentInspector, { ...fileProps, ontrash });

  expect(screen.queryByRole("button", { name: "Document actions" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
  expect(ontrash).toHaveBeenCalledOnce();
});

it("keeps identifiers behind a Details disclosure", () => {
  render(DocumentInspector, fileProps);
  const disclosure = screen.getByText("Details").closest("details")!;
  expect(disclosure.open).toBe(false);
  expect(disclosure.textContent).toContain("text/plain");
  expect(disclosure.textContent).not.toContain("charset");
  expect(disclosure.textContent).toContain("c".repeat(64).slice(0, 8));
  expect(screen.getByText("Path").closest("details")).toBeNull();
  expect(screen.getByText("Size").closest("details")).toBeNull();
});

it.each([
  { kind: "file", state: "another move is pending", trashing: true, actionsDisabled: false },
  { kind: "dir", state: "another move is pending", trashing: true, actionsDisabled: false },
  { kind: "file", state: "the view needs a refresh", trashing: false, actionsDisabled: true },
] as const)("disables $kind trash while $state", async ({ kind, trashing, actionsDisabled }) => {
  const ontrash = vi.fn();
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const props = {
    ...fileProps,
    node: { ...fileProps.node, kind },
    ontrash,
    trashing,
    actionsDisabled,
  };
  render(DocumentInspector, props);
  if (kind === "dir") {
    await fireEvent.click(screen.getByRole("button", { name: "Document actions" }));
    expect(
      screen.getByRole("menuitem", { name: "Move to trash" }).getAttribute("aria-disabled"),
    ).toBe("true");
  } else {
    expect(
      (screen.getByRole("button", { name: "Move to trash" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  }
});

it("removes an assigned tag directly and disables removal while tags are unavailable", async () => {
  const onremovetag = vi.fn();
  const tag = { id: "tag-review", name: "Review", revision: 1, assignment_count: 1 };
  const props = { ...fileProps, tags: [tag], onremovetag };
  const view = render(DocumentInspector, props);
  await fireEvent.click(screen.getByRole("button", { name: "Remove tag Review" }));
  expect(onremovetag).toHaveBeenCalledExactlyOnceWith(tag);
  await view.rerender({ ...props, tagsDisabled: true });
  expect(
    (screen.getByRole("button", { name: "Remove tag Review" }) as HTMLButtonElement).disabled,
  ).toBe(true);
});

it("reuses matching PDF facts, fetches missing or historical facts, and discards a late response", async () => {
  const api = await import("./generated/docbank.js");
  const { selectedSourceFromNode } = await import("./selectedSource.js");
  const file = {
    ...folder,
    kind: "file" as const,
    name: "contract.pdf",
    path: "/contract.pdf",
    mime_type: "application/pdf",
    size: 651,
    current_version_id: "11111111-1111-4111-8111-111111111111",
    blob_hash: "a".repeat(64),
  };
  const source = selectedSourceFromNode(file, file.path);

  const metadata: NonNullable<ContentVersion["source_metadata"]> = {
    contract_version: "source-metadata/v1",
    checksum: "c".repeat(64),
    extractor_fingerprint: "d".repeat(64),
    attachment: { node_id: 42, content_version_id: "22222222-2222-4222-8222-222222222222" },
    fields: [
      { source_field: "encrypted", value: { kind: "boolean" as const, boolean: false } },
      { source_field: "signature_field_count", value: { kind: "integer" as const, integer: 1 } },
      {
        source_field: "last_signature_covers_file",
        value: { kind: "boolean" as const, boolean: false },
      },
      { source_field: "embedded_file_count", value: { kind: "integer" as const, integer: 0 } },
      { source_field: "has_javascript", value: { kind: "boolean" as const, boolean: false } },
      { source_field: "has_launch_actions", value: { kind: "boolean" as const, boolean: false } },
    ].map((field) => ({
      ...field,
      key: `pdf.info.${field.source_field}`,
      namespace: "pdf.info",
      sensitive: false,
    })),
    warnings: [],
  };
  const current: ContentVersion = {
    id: "22222222-2222-4222-8222-222222222222",
    node_id: 42,
    blob_hash: "b".repeat(64),
    introduced_operation_id: "33333333-3333-4333-8333-333333333333",
    node_revision: 2,
    recorded_at: "2026-01-01T00:00:00Z",
    size: 651,
    transition_kind: "content_replace",
    source_metadata: metadata,
  };
  const previous: ContentVersion = {
    ...current,
    id: file.current_version_id,
    blob_hash: file.blob_hash,
    node_revision: 1,
    introduced_operation_id: "44444444-4444-4444-8444-444444444444",
    transition_kind: "content_create",
    source_metadata: {
      ...metadata,
      checksum: "e".repeat(64),
      attachment: { node_id: 42, content_version_id: file.current_version_id },
      fields: metadata.fields.map((field) =>
        field.key === "pdf.info.last_signature_covers_file"
          ? { ...field, value: { ...field.value, boolean: true } }
          : field,
      ),
    },
  };
  let finish!: (value: ContentVersion) => void;
  const request = vi
    .spyOn(api, "getContentVersion")
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    )
    .mockResolvedValueOnce(current);
  const props = {
    node: file,
    path: file.path,
    tags: [],
    tagsLoading: false,
    tagsError: "",
    source,
    session: "synthetic-session",
    onprovenance: vi.fn(),
    onhistory: vi.fn(),
  };
  const enriched = { ...file, source_metadata: previous.source_metadata };
  const view = render(DocumentInspector, { ...props, node: enriched });
  expect(request).not.toHaveBeenCalled();
  expect(screen.getByRole("region", { name: "PDF facts" }).textContent).toContain(
    "Covers the whole file",
  );
  await view.rerender(props);
  await vi.waitFor(() => expect(request).toHaveBeenCalledOnce());
  await view.rerender({
    ...props,
    node: enriched,
    source: {
      ...source,
      key: "second",
      versionID: "22222222-2222-4222-8222-222222222222",
      blobHash: "b".repeat(64),
      kind: "snapshot" as const,
      observedAt: "2026-01-01",
      originalTags: [],
      collectionIDs: [],
    },
  });
  expect((await screen.findByRole("region", { name: "PDF facts" })).textContent).toContain(
    "Does not cover the whole file",
  );

  expect(screen.getByRole("region", { name: "PDF facts" }).textContent).not.toContain(
    "Permissions",
  );
  expect(request.mock.calls[1]?.[0]).toBe("22222222-2222-4222-8222-222222222222");
  finish(previous);
  await Promise.resolve();
  await tick();
  expect(screen.getByRole("region", { name: "PDF facts" }).textContent).toContain(
    "Does not cover the whole file",
  );
});
