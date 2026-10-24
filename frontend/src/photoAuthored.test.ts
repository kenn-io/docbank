import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, render, screen, fireEvent, within } from "@testing-library/svelte";
import userEvent from "@testing-library/user-event";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { PhotoAuthoredEditor, PhotoUndoState } from "./photoAuthored.svelte.js";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
import { preparedPhoto } from "./photo-test-fixtures.js";
import PhotoInspector from "./PhotoInspector.svelte";
import type { PhotoAsset, PhotoPreparedTarget, Tag } from "./generated/docbank.js";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

const targets = [
  preparedPhoto(1, {
    asset_id: "asset",
    file_id: "raw",
    file_name: "capture.raw",
    values: { ...preparedPhoto(1).values, rating: 4 },
  }),
  preparedPhoto(2, {
    asset_id: "asset",
    file_id: "jpeg",
    file_name: "capture.jpg",
    values: { ...preparedPhoto(2).values, rating: 2 },
  }),
];
const asset = {
  id: "asset",
  display_file_id: "jpeg",
  files: [
    { id: "raw", role: "raw" },
    { id: "jpeg", role: "image" },
  ],
} as PhotoAsset;
const response = (body: unknown, status = 200) => Response.json(body, { status });
function editorWithTargets() {
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  editor.targets = structuredClone(targets);
  editor.asset = structuredClone(asset);
  return editor;
}
function ui(editor: PhotoAuthoredEditor, onedit = vi.fn()) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  Element.prototype.scrollIntoView = vi.fn();
  return render(PhotoInspector, { editor, onedit });
}

it.each([
  { behavior: "clear fields", name: "stages cleared caption, creator, and copyright fields" },
  { behavior: "decisions and keywords", name: "stages pair decisions and keywords behind Apply" },
  { behavior: "member scope", name: "releases pair member selection after Clear" },
])("$name", async ({ behavior }) => {
  const editor = editorWithTargets(),
    onedit = vi.fn();
  editor.targets[0]!.values.caption = "RAW caption";
  editor.targets[0]!.values.creator = "Photographer";
  editor.targets[0]!.values.copyright = "Owner";
  editor.targets[0]!.tags = ["shared", "mixed"];
  editor.targets[1]!.tags = ["shared"];
  editor.tags = [
    { id: "shared", name: "Portfolio" },
    { id: "mixed", name: "Travel" },
    { id: "tag", name: "Exhibition" },
  ] as Tag[];
  if (behavior !== "clear fields") editor.stage({ caption: "", creator: "", copyright: "" });
  ui(editor, onedit);
  if (behavior === "clear fields") {
    for (const field of ["caption", "creator", "copyright"] as const) {
      await fireEvent.click(screen.getByRole("button", { name: `Clear ${field}` }));
      expect(editor.patch[field]).toBe("");
    }
  } else {
    if (behavior === "decisions and keywords") {
      expect(screen.getByLabelText("Assigned keywords").textContent).toBe(
        "PortfolioTravel · Mixed",
      );
      expect(editor.value("rating")).toBeUndefined();
      await fireEvent.click(screen.getByRole("combobox", { name: /Rating:/ }));
      await fireEvent.click(screen.getByRole("option", { name: "5 stars" }));
      await fireEvent.click(screen.getByRole("combobox", { name: /Keyword:/ }));
      await fireEvent.click(screen.getByRole("option", { name: /Exhibition/ }));
      await fireEvent.click(screen.getByRole("button", { name: "Add keyword" }));
      expect(onedit).not.toHaveBeenCalled();
      expect(editor.patch).toEqual({ caption: "", creator: "", copyright: "", rating: 5 });
      expect(editor.keywords).toEqual([{ tag_id: "tag", assign: true }]);
      await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
      expect(onedit).toHaveBeenCalledWith({});
    } else {
      await fireEvent.click(screen.getByRole("button", { name: "Clear edits" }));
      await fireEvent.click(screen.getByRole("combobox", { name: /Edit members/ }));
      expect(screen.getByRole("option", { name: "RAW · capture.raw" })).toBeTruthy();
      expect(screen.getByRole("option", { name: "IMAGE · capture.jpg" })).toBeTruthy();
      await fireEvent.click(screen.getByRole("option", { name: "RAW · capture.raw" }));
      expect(editor.files).toHaveLength(1);
      expect(editor.value("rating")).toBe(4);
      expect(editor.dirty).toBe(false);
    }
  }
  editor.dispose();
});

it("preserves multiline caption through rendered editing", async () => {
  const editor = editorWithTargets(),
    stored = "First line\nSecond line",
    edited = "First line\nEdited line\nThird line";
  for (const target of editor.targets) target.values.caption = stored;
  ui(editor);
  const control = screen.getByRole("textbox", { name: "Caption" }) as HTMLTextAreaElement;
  expect(control.value).toBe(stored);
  const user = userEvent.setup();
  await user.click(control);
  control.setSelectionRange(0, control.value.length);
  await user.paste(edited);
  expect(document.activeElement).toBe(control);
  expect(control.value).toBe(edited);
  expect(editor.patch.caption).toBe(edited);
  editor.dispose();
});

it.each([
  {
    name: "whole pair scope",
    fileID: "",
    expected: [
      {
        asset_id: "asset",
        asset_revision: 1,
        file_id: "raw",
        revision: 1,
        node_revision: 1,
        content_version_id: "viewed-version",
      },
      {
        asset_id: "asset",
        asset_revision: 1,
        file_id: "jpeg",
        revision: 1,
        node_revision: 1,
        content_version_id: "viewed-version",
      },
    ],
  },
  {
    name: "RAW scope narrowed via fileID",
    fileID: "raw",
    expected: [
      {
        asset_id: "asset",
        asset_revision: 1,
        file_id: "raw",
        revision: 1,
        node_revision: 1,
        content_version_id: "viewed-version",
      },
    ],
  },
])("binds the viewed content version without changing $name", async ({ fileID, expected }) => {
  const editor = editorWithTargets();
  editor.fileID = fileID;
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockResolvedValue({ receipt_id: "", before: [], after: [] });
  await editor.edit({}, { flag: "pick" }, false, vi.fn(), {
    viewed: { content_version_id: "viewed-version" },
  });
  expect(edit).toHaveBeenCalledOnce();
  expect(edit.mock.calls[0]![0].targets).toEqual(expected);
  expect(editor.fileID).toBe(fileID);
  expect(editor.conflict).toBe(false);
});

it("restores the pre-edit draft when a viewed decision is refused", async () => {
  const editor = editorWithTargets();
  editor.stage({ caption: "River" });
  vi.spyOn(api, "editPhotoAuthored").mockRejectedValueOnce(
    new APIError("Photo changed", 412, "stale_revision"),
  );
  await editor.edit({}, { flag: "pick" }, false, vi.fn(), {
    viewed: { content_version_id: "viewed-version" },
    rotationDelta: 90,
  });
  expect(editor.conflict).toBe(true);
  expect(editor.patch).toEqual({ caption: "River" });
  expect(editor.dirty).toBe(true);
  expect(editor.rotationDelta).toBeUndefined();
});

it("merges a shortcut into the staged draft", async () => {
  const editor = editorWithTargets();
  editor.fileID = "jpeg";
  editor.stage({ caption: "River" }, [{ tag_id: "tag", assign: true }]);
  const fetcher = vi.fn().mockResolvedValue(
    response({
      receipt_id: "receipt",
      after: [
        {
          file_id: "jpeg",
          revision: 2,
          values: { ...targets[1]!.values, rating: 5, caption: "River" },
        },
      ],
      keywords: [{ node_id: 2, tag_id: "tag", after: true }],
      keyword_nodes: [{ node_id: 2, after_revision: 3 }],
    }),
  );
  vi.stubGlobal("fetch", fetcher);
  await editor.edit({ asset_ids: ["asset"] }, { rating: 5 }, false, vi.fn());
  const sent = JSON.parse(fetcher.mock.calls[0][1].body);
  expect(sent.targets).toHaveLength(1);
  expect(sent.targets[0].file_id).toBe("jpeg");
  expect(sent.patch).toEqual({ caption: "River", rating: 5 });
  expect(sent.keywords).toEqual([{ tag_id: "tag", assign: true }]);
  expect(editor.receiptID).toBe("receipt");
  expect(editor.dirty).toBe(false);
  expect(editor.undoState.summary).toBe("1 file updated: caption, rating, keywords");
});

it("sends keyboard rotation as one atomic relative edit for each member", async () => {
  const editor = editorWithTargets();
  editor.targets[0]!.values.rotation = 90;
  editor.targets[1]!.values.rotation = 180;
  const fetcher = vi.fn().mockResolvedValue(
    response({
      receipt_id: "rotated",
      after: editor.targets.map((target) => ({
        file_id: target.file_id,
        revision: 2,
        values: { ...target.values, rotation: target.values.rotation + 90 },
      })),
    }),
  );
  vi.stubGlobal("fetch", fetcher);
  await editor.edit({ asset_ids: ["asset"] }, {}, false, vi.fn(), { rotationDelta: 90 });
  const sent = JSON.parse(fetcher.mock.calls[0][1].body);
  expect(sent.rotation_delta).toBe(90);
  expect(sent.patch).toEqual({});
  expect(editor.dirty).toBe(false);
  expect(editor.receiptID).toBe("rotated");
});

it("holds a file prepared by both the edit selection and the stack once", async () => {
  const editor = editorWithTargets(),
    sibling = preparedPhoto(3, { asset_id: "other", file_id: "other-file" });
  const fetcher = vi
    .fn()
    .mockResolvedValueOnce(
      response({ assets: [], targets: [...structuredClone(targets), sibling] }),
    )
    .mockResolvedValueOnce(
      response({
        receipt_id: "rated",
        after: [{ file_id: "raw", revision: 9, values: { ...targets[0]!.values, rating: 5 } }],
      }),
    );
  vi.stubGlobal("fetch", fetcher);
  await editor.prepareStack(["asset", "other"], false);
  expect(editor.prepared.map((target) => target.file_id).toSorted()).toEqual([
    "jpeg",
    "other-file",
    "raw",
  ]);
  await editor.edit({ asset_ids: ["asset"] }, { rating: 5 }, false, vi.fn());
  const revisions = (list: typeof editor.targets) =>
    list.filter((target) => target.file_id === "raw").map((target) => target.revision);
  expect(revisions(editor.targets)).toEqual([9]);
  expect(revisions(editor.stackTargets)).toEqual([9]);
  expect(editor.prepared).toHaveLength(3);
  editor.reset();
  expect(editor.prepared).toEqual([]);
});

it("leaves shortcut navigation usable when preparing its targets fails", async () => {
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(response({ detail: "Refresh your selection" }, 412)),
  );
  await editor.edit({ asset_ids: ["asset"] }, { rating: 5 }, false, vi.fn());
  expect(editor.error).toBe("Refresh your selection");
  expect(editor.dirty).toBe(false);
  expect(editor.canChangeTarget()).toBe(true);
});

it("reuses prepared search targets with their latest revisions", async () => {
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  editor.tags = [{ id: "tag", name: "Portfolio" }] as Tag[];
  const fetcher = vi
    .fn()
    .mockResolvedValueOnce(response({ assets: [asset], targets }))
    .mockResolvedValueOnce(
      response({
        receipt_id: "first",
        after: targets.map((t) => ({
          file_id: t.file_id,
          revision: 2,
          values: { ...t.values, rating: 5 },
        })),
      }),
    )
    .mockResolvedValueOnce(
      response({
        receipt_id: "second",
        after: targets.map((t) => ({
          file_id: t.file_id,
          revision: 3,
          values: { ...t.values, rating: 5, flag: "pick" },
        })),
      }),
    );
  vi.stubGlobal("fetch", fetcher);
  const query = { text: "", syntax: "simple" as const, filters: { rating_max: 0 } };
  await editor.edit({ query }, { rating: 5 }, false, async () => {});
  await editor.edit({ asset_ids: [] }, { flag: "pick" }, false, async () => {});
  expect(fetcher.mock.calls.filter(([url]) => String(url).endsWith("/prepare"))).toHaveLength(1);
  const sent = JSON.parse(fetcher.mock.calls[2][1].body);
  expect(
    sent.targets.map((t: PhotoPreparedTarget) => [t.file_id, t.revision, t.node_revision]),
  ).toEqual([
    ["raw", 2, 2],
    ["jpeg", 2, 2],
  ]);
});

it("aborts pending preparation when visibility must be revalidated", async () => {
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  let finish!: (response: Response) => void;
  const fetcher = vi.fn(
    (_url: string, _init: RequestInit) => new Promise<Response>((resolve) => (finish = resolve)),
  );
  vi.stubGlobal("fetch", fetcher);
  const pending = editor.prepare({ asset_ids: ["asset"] });
  editor.conceal();
  expect(fetcher.mock.calls[0]![1].signal!.aborted).toBe(true);
  finish(response({ assets: [asset], targets }));
  await pending;
  expect(editor.targets).toHaveLength(0);
  expect(editor.asset).toBeUndefined();
  expect(editor.validating).toBe(true);
  editor.revalidate(new Set());
  expect(editor.validating).toBe(false);
  editor.dispose();
});

async function reloadPreparedMembers(
  selected: "unchanged" | "raw" | "jpeg" | "display trashed" | "revalidated",
) {
  const editor = editorWithTargets();
  editor.fileID =
    selected === "unchanged" || selected === "display trashed" || selected === "revalidated"
      ? ""
      : selected;
  if (selected === "display trashed") editor.targets[1]!.asset_id = "other-asset";
  editor.tags = [{ id: "tag", name: "Portfolio" }] as Tag[];
  const added = { ...targets[0]!, file_id: "added", file_name: "new.jpg" };
  const remaining = { ...editor.targets[1]!, revision: 2, node_revision: 3 };
  const writes = selected === "revalidated" ? 0 : 1;
  const fetcher = vi.fn();
  if (writes)
    fetcher.mockResolvedValueOnce(
      response(
        { detail: "Photo changed" },
        selected === "unchanged" ? 412 : selected === "display trashed" ? 409 : 404,
      ),
    );
  fetcher
    .mockResolvedValueOnce(
      response({
        assets: [
          {
            ...asset,
            id: remaining.asset_id,
            files: [
              { id: "jpeg", role: "image" },
              { id: "added", role: "image" },
            ],
          },
        ],
        targets:
          selected === "unchanged"
            ? targets.map((t) => ({ ...t, revision: 2, node_revision: 3 }))
            : [remaining, added],
      }),
    )
    .mockResolvedValueOnce(response({ receipt_id: "retry", after: [] }));
  vi.stubGlobal("fetch", fetcher);
  if (selected === "revalidated") {
    editor.targets = [...editor.targets, preparedPhoto(3)];
    editor.stage({ caption: "River" }, [{ tag_id: "tag", assign: true }]);
    editor.revalidate(new Set(["asset"]));
    expect(editor.membershipChanged).toBe(false);
  } else
    await editor.edit({ asset_ids: ["asset"] }, { caption: "River" }, false, vi.fn(), {
      keywords: [{ tag_id: "tag", assign: true }],
    });
  expect(editor.conflict).toBe(true);
  expect(editor.dirty).toBe(true);
  await editor.edit({}, { rating: 5 }, false, vi.fn(), {
    keywords: [{ tag_id: "other", assign: true }],
  });
  expect(fetcher).toHaveBeenCalledTimes(writes);
  ui(editor);
  expect((screen.getByRole("combobox", { name: /Rating:/ }) as HTMLButtonElement).disabled).toBe(
    true,
  );
  expect((screen.getByRole("textbox", { name: "Caption" }) as HTMLInputElement).disabled).toBe(
    true,
  );
  if (selected === "revalidated")
    expect(screen.queryByRole("button", { name: "Use reviewed members" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Reload metadata" }));
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(writes + 1));
  expect(JSON.parse(fetcher.mock.calls[writes][1].body)).toEqual({
    asset_ids: selected === "display trashed" ? ["asset", "other-asset"] : ["asset"],
    skip_unavailable: true,
  });
  return { editor, fetcher, remaining, writes };
}

async function applyReviewedMembers(
  selected: string,
  recovery: Awaited<ReturnType<typeof reloadPreparedMembers>>,
) {
  const { editor, fetcher, remaining, writes } = recovery;
  const review = await screen.findByRole("button", { name: "Use reviewed members" });
  expect(editor.targets).toEqual([remaining]);
  expect(editor.fileID).toBe(selected === "jpeg" ? "jpeg" : "");
  expect(editor.asset!.files.map((file) => file.id)).toEqual(["jpeg"]);
  expect(screen.getByRole("list", { name: "Members to review" }).textContent).toBe("capture.jpg");
  expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(true);
  await editor.edit({}, { rating: 5 }, false, vi.fn());
  expect(fetcher).toHaveBeenCalledTimes(writes + 1);
  expect(editor.patch).toEqual({ caption: "River" });
  expect(editor.keywords).toEqual([{ tag_id: "tag", assign: true }]);
  await fireEvent.click(review);
  expect(editor.conflict).toBe(false);
  await editor.edit({}, {}, false, vi.fn());
  const sent = JSON.parse(fetcher.mock.calls[writes + 1][1].body);
  expect(sent.targets).toEqual([
    {
      asset_id: remaining.asset_id,
      asset_revision: 1,
      file_id: "jpeg",
      revision: 2,
      node_revision: 3,
    },
  ]);
  expect(sent.patch).toEqual({ caption: "River" });
  expect(sent.keywords).toEqual([{ tag_id: "tag", assign: true }]);
  editor.dispose();
}

it("keeps Clear available when recovery removes every prepared photo", async () => {
  const editor = editorWithTargets();
  editor.stage({ caption: "River" });
  editor.revalidate(new Set());
  ui(editor);
  expect(
    screen.getByText("No photos are available to review. Clear edits to discard this draft."),
  ).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Reload metadata" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Clear edits" }));
  expect(editor.dirty).toBe(false);
  editor.dispose();
});

it.each(["raw", "jpeg", "display trashed", "revalidated", "unchanged"] as const)(
  "retains drafts through prepared-member recovery, members=%s",
  async (selected) => {
    const recovery = await reloadPreparedMembers(selected);
    if (selected === "unchanged") {
      const { editor, fetcher, writes } = recovery;
      await vi.waitFor(() => expect(editor.conflict).toBe(false));
      expect(editor.patch).toEqual({ caption: "River" });
      expect(editor.keywords).toEqual([{ tag_id: "tag", assign: true }]);
      await editor.edit({}, {}, false, vi.fn());
      expect(JSON.parse(fetcher.mock.calls[writes + 1][1].body).targets[0].node_revision).toBe(3);
      editor.dispose();
    } else await applyReviewedMembers(selected, recovery);
  },
);

it("hides metadata fields while concealed", () => {
  const editor = editorWithTargets();
  editor.targets[0]!.values.caption = "Saved caption";
  editor.targets[0]!.tags = ["tag"];
  editor.tags = [{ id: "tag", name: "Private keyword" }] as Tag[];
  editor.stage({ caption: "Draft caption" });
  editor.concealed = true;
  ui(editor);
  expect(screen.getByText("Metadata unavailable")).toBeTruthy();
  expect(screen.queryByRole("textbox", { name: "Caption" })).toBeNull();
  expect(screen.queryByText("Private keyword")).toBeNull();
  editor.dispose();
});

it.each(["write", "draft", "loading"])(
  "refuses edits and target changes while a %s is pending",
  async (kind) => {
    const editor = new PhotoAuthoredEditor("scoped", vi.fn());
    if (kind === "write") {
      editor.busy = true;
      await editor.edit({}, { rating: 5 }, false, vi.fn());
      expect(editor.notice).toBe(
        "An edit is in progress. Try the shortcut again when it finishes.",
      );
    } else if (kind === "loading") editor.loading = true;
    else {
      editor.targets = targets;
      editor.stage({ caption: "Draft" });
    }
    expect(editor.canChangeTarget()).toBe(false);
    expect(editor.notice).toContain(
      kind === "write"
        ? "Wait for it to finish"
        : kind === "loading"
          ? "Metadata is loading. Try again when it finishes."
          : "Apply or clear your metadata edits",
    );
    expect(editor.noticeTone).toBe("warning");
    editor.busy = false;
    editor.loading = false;
    if (kind === "draft") editor.clearDraft();
    expect(editor.canChangeTarget()).toBe(true);
    editor.dispose();
  },
);

it.each(["pending", "failed"])(
  "keeps metadata editable while keyword loading is %s",
  async (state) => {
    const editor =
      state === "pending" ? new PhotoAuthoredEditor("session", vi.fn()) : editorWithTargets();
    const prepared = editor.targets;
    let finish!: () => void;
    if (state === "pending") {
      editor.loadTags = vi.fn(
        () =>
          new Promise<void>((resolve) => {
            editor.tagsLoading = true;
            finish = () => {
              editor.tagsLoading = false;
              resolve();
            };
          }),
      );
      vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response({ assets: [asset], targets })));
      await editor.prepare({ asset_ids: ["asset"] });
      expect(editor.loadTags).toHaveBeenCalledOnce();
      expect(editor.tagsLoading).toBe(true);
      expect(editor.targets).toEqual(targets);
      expect(editor.loading).toBe(false);
      expect(editor.error).toBe("");
    } else {
      editor.tagsError = "Keywords unavailable";
      editor.stage({ caption: "Draft caption" });
      editor.loadTags = vi.fn(async () => {
        editor.tagsError = "";
        editor.catalog([
          { id: "tag", name: "Portfolio", revision: 2, assignment_count: 1 },
        ] as Tag[]);
      });
    }
    ui(editor);
    expect((screen.getByRole("textbox", { name: "Caption" }) as HTMLInputElement).disabled).toBe(
      false,
    );
    if (state === "failed") {
      expect(screen.getByText("Keywords unavailable")).toBeTruthy();
      await fireEvent.click(screen.getByRole("button", { name: "Retry keywords" }));
      expect(editor.loadTags).toHaveBeenCalledOnce();
      expect(editor.targets).toBe(prepared);
      expect(editor.patch.caption).toBe("Draft caption");
      expect(screen.queryByRole("button", { name: "Retry keywords" })).toBeNull();
      await fireEvent.click(screen.getByRole("combobox", { name: /Keyword:/ }));
      expect(screen.getByRole("option", { name: /Portfolio/ }).textContent).toContain("1");
    } else finish();
    editor.dispose();
  },
);

it.each(["shared receipt", "no-op edit", "empty Undo"])(
  "preserves receipt state for %s",
  async (behavior) => {
    const state = new PhotoUndoState(),
      first = new PhotoAuthoredEditor("session", vi.fn(), state);
    first.targets = structuredClone(targets);
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response({ receipt_id: "first", after: targets }))
      .mockResolvedValueOnce(response({ receipt_id: "latest", after: targets }));
    vi.stubGlobal("fetch", fetcher);
    const refresh = vi.fn();
    await first.edit({}, { rating: 5 }, false, refresh);
    await first.edit({}, { flag: "pick" }, false, refresh);
    refresh.mockClear();
    if (behavior === "no-op edit") {
      fetcher.mockResolvedValueOnce(response({ receipt_id: "", before: [], after: [] }));
      await first.edit({}, { flag: "pick" }, false, refresh);
      expect(first.dirty).toBe(false);
      expect(first.receiptID).toBe("latest");
      expect(first.notice).toBe("No changes to apply");
      expect(refresh).not.toHaveBeenCalled();
    } else if (behavior === "shared receipt") {
      fetcher
        .mockResolvedValueOnce(response({ detail: "Changed elsewhere" }, 412))
        .mockResolvedValueOnce(response({ receipt_id: "inverse", after: targets }));
      first.stage({ caption: "Local draft" });
      first.dispose();
      const next = new PhotoAuthoredEditor("session", vi.fn(), state);
      expect(next.receiptID).toBe("latest");
      expect(next.dirty).toBe(false);
      await next.undo(refresh);
      expect(next.receiptID).toBe("latest");
      expect(next.error).toBe("Changed elsewhere");
      expect(refresh).not.toHaveBeenCalled();
      await next.undo(refresh);
      expect(fetcher.mock.calls[2][0]).toContain("/latest/undo");
      expect(refresh).toHaveBeenCalledOnce();
      expect(state.receiptID).toBe("");
      expect(state.summary).toBe("");
      next.dispose();
    } else {
      fetcher.mockResolvedValueOnce(response({ receipt_id: "", before: [], after: [] }));
      first.receiptID = "empty";
      first.undoState.summary = "1 file updated: rating";
      await first.undo(refresh);
      expect(first.notice).toBe("No changes to undo");
      expect(first.receiptID).toBe("");
      expect(first.undoState.summary).toBe("");
      expect(refresh).not.toHaveBeenCalled();
    }
    first.dispose();
  },
);

it("shows field diagnostics for a failed edit and keeps its draft and Undo", async () => {
  const editor = editorWithTargets();
  editor.receiptID = "prior";
  vi.spyOn(api, "editPhotoAuthored").mockRejectedValue(
    new APIError("validation failed", 422, "validation", undefined, [
      "Caption is too long; field=body.patch.caption!",
    ]),
  );
  await editor.edit({}, { caption: "draft" }, false, vi.fn());
  expect(editor.error).toBe("Caption is too long; field=body.patch.caption!");
  expect(editor.patch).toEqual({ caption: "draft" });
  expect(editor.receiptID).toBe("prior");
  editor.dispose();
});

it.each(["catalog first", "preparation first"])(
  "removes a deleted keyword from choices and preserves the draft, %s",
  async (order) => {
    const editor = editorWithTargets();
    const remaining = { id: "remaining", name: "Portfolio" } as Tag;
    editor.targets[0]!.tags = ["deleted"];
    editor.catalog([{ id: "deleted", name: "Gone" } as Tag, remaining]);
    let finish!: (value: api.PhotoAuthoredPreparation) => void;
    vi.spyOn(api, "preparePhotoAuthored").mockImplementation(
      () => new Promise((resolve) => (finish = resolve)),
    );
    vi.spyOn(api, "getTag").mockRejectedValue(new APIError("Missing", 404, "not_found"));
    const preparation = editor.prepare({ asset_ids: ["asset"] });
    editor.stage({ caption: "Draft caption", rating: 5 });
    const refreshCatalog = async () => {
      editor.catalog([remaining]);
      await editor.refreshAssignedTags();
    };
    if (order === "catalog first") await refreshCatalog();
    finish({ assets: [asset], targets: structuredClone(targets) });
    await preparation;
    if (order === "preparation first") await refreshCatalog();
    expect(editor.targets.flatMap((target) => target.tags)).toEqual([]);
    expect(editor.tags).toEqual([remaining]);
    expect(editor.patch).toEqual({ caption: "Draft caption", rating: 5 });
    ui(editor);
    await fireEvent.click(screen.getByRole("combobox", { name: /Keyword:/ }));
    expect(screen.queryByRole("option", { name: /Gone/ })).toBeNull();
    expect(screen.getByRole("option", { name: /Portfolio/ })).toBeTruthy();
    editor.dispose();
  },
);

it("keeps newer assigned tag reads and retains choices on errors", async () => {
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  const tag = (id: string, name: string, revision = 1) =>
    ({ id, name, revision, assignment_count: 1 }) as Tag;
  editor.targets = [preparedPhoto(1, { tags: ["renamed"] })];
  let older!: () => void;
  const fetcher = vi.fn(async (url: string) =>
    url.endsWith("/tags/renamed")
      ? response(tag("renamed", "New name", 2))
      : response({ detail: "Missing" }, 404),
  );
  fetcher.mockImplementationOnce(
    () =>
      new Promise<Response>(
        (resolve) => (older = () => resolve(response(tag("renamed", "Old name")))),
      ),
  );
  vi.stubGlobal("fetch", fetcher);
  editor.catalog([]);
  await vi.waitFor(() => expect(older).toBeDefined());
  editor.tags = [tag("renamed", "Old name"), tag("other", "Other")];
  await editor.refreshAssignedTags();
  expect(editor.tags.map((value) => value.name)).toEqual(["New name", "Other"]);
  older();
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(editor.tags.map((value) => value.name)).toEqual(["New name", "Other"]);
  fetcher.mockImplementation(async () => response({ detail: "Down" }, 503));
  await expect(editor.refreshAssignedTags()).rejects.toThrow("Down");
  expect(editor.tags.map((value) => value.name)).toEqual(["New name", "Other"]);
  editor.dispose();
});

it("retries a live keyword read outside the catalog before consuming the pending change", async () => {
  const editor = editorWithTargets();
  editor.tags = [{ id: "catalog", name: "Catalog keyword" }] as Tag[];
  const latest = targets.map((target) => ({ ...target, tags: ["external"] }));
  const keyword = { id: "external", name: "External keyword" } as Tag;
  let reads = 0;
  let prepares = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      if (url.endsWith("/prepare")) {
        prepares++;
        return response({ assets: [asset], targets: latest });
      }
      return ++reads === 1 ? response({ detail: "Keywords unavailable" }, 503) : response(keyword);
    }),
  );
  try {
    await expect(editor.invalidate({ ids: new Set(["asset"]), idFree: false })).rejects.toThrow(
      "Keywords unavailable",
    );
    expect(editor.tags.map((tag) => tag.id)).toEqual(["catalog"]);
    // The watcher retries the same identified change after a failed read.
    await editor.invalidate({ ids: new Set(["asset"]), idFree: false });
    expect(editor.tags).toContainEqual(keyword);
    expect(reads).toBe(2);
    await editor.invalidate({ ids: new Set(["other-asset"]), idFree: false });
    expect(prepares).toBe(2);
  } finally {
    editor.dispose();
  }
});

it.each([false, true])(
  "refreshes after Clear edits when a draft discarded a live response, failed=%s",
  async (failed) => {
    const editor = editorWithTargets();
    const latest = targets.map((target) => ({
      ...target,
      revision: 2,
      values: { ...target.values, caption: "Saved update" },
    }));
    let finish!: (response: Response) => void;
    const fetcher = vi
      .fn()
      .mockImplementationOnce(() => new Promise<Response>((resolve) => (finish = resolve)))
      .mockImplementation(async () => response({ assets: [asset], targets: latest }));
    vi.stubGlobal("fetch", fetcher);
    const refreshing = editor.invalidate({ ids: new Set(["asset"]), idFree: false });
    await vi.waitFor(() => expect(finish).toBeDefined());
    editor.stage({ caption: "Draft" });
    finish(response({ assets: [asset], targets: latest }));
    await refreshing;
    expect(editor.value("caption")).toBe("Draft");
    expect(editor.targets.map((target) => target.revision)).toEqual([1, 1]);
    if (failed) {
      ui(editor);
      fetcher.mockRejectedValueOnce(new APIError("Metadata unavailable", 503, "unavailable"));
    }
    editor.clearDraft();
    if (failed) {
      await vi.waitFor(() => expect(editor.error).toBe("Metadata unavailable"));
      expect((screen.getByLabelText("Caption") as HTMLTextAreaElement).disabled).toBe(true);
      await fireEvent.click(screen.getByRole("button", { name: "Reload metadata" }));
    }
    await vi.waitFor(() => expect(editor.value("caption")).toBe("Saved update"));
    expect(editor.targets.map((target) => target.revision)).toEqual([2, 2]);
    editor.dispose();
  },
);

// A quiet read in flight when a write starts is dropped: it must neither hold the write up
// behind Hidden access nor land on revisions the write has already moved on.
function editAfterQuietRead(editor: PhotoAuthoredEditor) {
  const receipt = {
    receipt_id: "receipt",
    after: targets.map((target) => ({
      file_id: target.file_id,
      revision: 7,
      values: { ...target.values, rating: 5 },
    })),
  };
  let answer!: () => void;
  const fetcher = vi.fn((url: string, init: RequestInit) =>
    url.endsWith("/prepare")
      ? new Promise<Response>((resolve, reject) => {
          answer = () => resolve(response({ assets: [asset], targets: structuredClone(targets) }));
          const abort = () => reject(new DOMException("", "AbortError"));
          if (init.signal!.aborted) abort();
          else init.signal!.addEventListener("abort", abort);
        })
      : Promise.resolve(response(receipt)),
  );
  vi.stubGlobal("fetch", fetcher);
  void editor.reload(false, true);
  const edit = editor.edit({ asset_ids: ["asset"] }, { rating: 5 }, false, vi.fn());
  return { edit, fetcher, answer: () => answer() };
}

it.each([false, true])(
  "drops a pending quiet metadata read when an edit starts (Hidden access: %s)",
  async (hidden) => {
    const editor = new PhotoAuthoredEditor(
      "session",
      vi.fn(),
      undefined,
      hidden ? new HiddenAccess("session", vi.fn()) : undefined,
    );
    editor.targets = structuredClone(targets);
    editor.asset = structuredClone(asset);
    const { edit, fetcher, answer } = editAfterQuietRead(editor);
    await vi.waitFor(() =>
      expect(fetcher.mock.calls.some(([url]) => String(url).endsWith("/edit"))).toBe(true),
    );
    await edit;
    answer();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(editor.targets.map((target) => target.revision)).toEqual([7, 7]);
  },
);

it.each(["assigned tags", "edit counts", "Undo counts"])(
  "refreshes keywords while preserving %s",
  async (behavior) => {
    const editor = new PhotoAuthoredEditor("session", vi.fn()),
      assigned = "40000000-0000-4000-8000-000000000001",
      catalogID = "tag-0";
    const catalog = Array.from({ length: 1001 }, (_, index) => ({
      id: `tag-${index}`,
      name: index ? `Keyword ${index}` : "Portfolio",
      revision: 1,
      assignment_count: 0,
    })) as Tag[];
    let assignedCount = 1;
    const fetcher = vi.fn(async (url: string) => {
      if (url.endsWith("/photos/authored/prepare"))
        return response({
          assets: [asset],
          targets: targets.map((target, index) => ({
            ...target,
            tags: index === 0 ? [assigned] : [],
          })),
        });
      if (url.endsWith(`/tags/${assigned}`))
        return response({
          id: assigned,
          name: "Beyond the catalog",
          revision: assignedCount,
          assignment_count: assignedCount,
        });
      if (url.endsWith(`/tags/${catalogID}`))
        return response({
          id: catalogID,
          name: "Portfolio",
          revision: assignedCount === 2 ? 2 : 3,
          assignment_count: assignedCount === 2 ? 2 : 0,
        });
      const undo = url.endsWith("/undo");
      assignedCount = undo ? 1 : 2;
      return response({
        receipt_id: undo ? "inverse" : "edit",
        before: targets,
        after: targets,
        keywords: [
          { node_id: 2, tag_id: assigned, before: undo, after: !undo },
          { node_id: 1, tag_id: catalogID, before: undo, after: !undo },
        ],
      });
    });
    vi.stubGlobal("fetch", fetcher);
    editor.catalog(catalog);
    await editor.prepare({ asset_ids: ["asset"] });
    ui(editor);
    await screen.findByText(/Beyond the catalog/);
    editor.catalog(catalog);
    const assertCount = async (name: RegExp, expected: number) => {
      await fireEvent.click(screen.getByRole("combobox", { name: /Keyword:/ }));
      const option = screen.getByRole("option", { name });
      expect(within(option).getByText(String(expected), { exact: true })).toBeTruthy();
      await fireEvent.click(option);
    };
    if (behavior === "assigned tags") {
      expect(fetcher.mock.calls.map(([url]) => url)).toEqual([
        "/api/v1/photos/authored/prepare",
        `/api/v1/tags/${assigned}`,
      ]);
      expect(editor.tags).toHaveLength(1002);
      expect(catalog).toHaveLength(1001);
      await assertCount(/Beyond the catalog/, 1);
    } else {
      const notify = vi.fn();
      await editor.edit({}, {}, false, notify, {
        keywords: [
          { tag_id: assigned, assign: true },
          { tag_id: catalogID, assign: true },
        ],
      });
      if (behavior === "edit counts") {
        expect(notify.mock.calls[0]?.[1]).toEqual(
          expect.arrayContaining([
            expect.objectContaining({ id: catalogID, revision: 2, assignment_count: 2 }),
          ]),
        );
        await assertCount(/Beyond the catalog/, 2);
        await assertCount(/Portfolio/, 2);
        expect(editor.tags.find((tag) => tag.id === catalogID)).toMatchObject({
          assignment_count: 2,
          revision: 2,
        });
      } else {
        await editor.undo(notify);
        expect(notify.mock.calls[1]?.[1]).toEqual(
          expect.arrayContaining([
            expect.objectContaining({ id: catalogID, revision: 3, assignment_count: 0 }),
          ]),
        );
        await assertCount(/Beyond the catalog/, 1);
        await assertCount(/Portfolio/, 0);
        expect(editor.tags.find((tag) => tag.id === catalogID)).toMatchObject({
          assignment_count: 0,
          revision: 3,
        });
        expect(editor.tags.find((tag) => tag.id === "tag-1000")).toEqual(catalog[1000]);
      }
    }
    editor.dispose();
  },
);

it.each(["edit", "Undo"])(
  "accepts the next edit while the %s refresh is still running",
  async (action) => {
    const editor = editorWithTargets(),
      fetcher = vi.fn(async () =>
        response({ receipt_id: "receipt", before: targets, after: targets }),
      );
    vi.stubGlobal("fetch", fetcher);
    let settle!: () => void;
    const refresh = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          settle = resolve;
        }),
    );
    if (action === "Undo") editor.receiptID = "earlier";
    const pending =
      action === "edit" ? editor.edit({}, { rating: 5 }, false, refresh) : editor.undo(refresh);
    await vi.waitFor(() => expect(refresh).toHaveBeenCalledOnce());
    await editor.edit(
      {},
      { rating: 3 },
      false,
      vi.fn(async () => {}),
    );
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(editor.notice).not.toMatch(/in progress/);
    settle();
    await pending;
    editor.dispose();
  },
);

it("keeps a later edit busy when an earlier refresh finishes during its write", async () => {
  const editor = editorWithTargets();
  let finishWrite!: (value: Response) => void;
  const fetcher = vi
    .fn()
    .mockResolvedValueOnce(response({ receipt_id: "first", before: targets, after: targets }))
    .mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          finishWrite = resolve;
        }),
    );
  vi.stubGlobal("fetch", fetcher);
  let finishRefresh!: () => void;
  const first = editor.edit(
    {},
    { rating: 5 },
    false,
    () =>
      new Promise<void>((resolve) => {
        finishRefresh = resolve;
      }),
  );
  await vi.waitFor(() => expect(finishRefresh).toBeTypeOf("function"));
  const second = editor.edit(
    {},
    { rating: 3 },
    false,
    vi.fn(async () => {}),
  );
  await vi.waitFor(() => expect(finishWrite).toBeTypeOf("function"));
  finishRefresh();
  await first;
  expect(editor.busy).toBe(true);
  expect(editor.canChangeTarget()).toBe(false);
  finishWrite(response({ receipt_id: "second", before: targets, after: targets }));
  await second;
  expect(editor.busy).toBe(false);
  editor.dispose();
});

it("reads external metadata retained while an edit response is pending", async () => {
  const editor = editorWithTargets();
  let finish!: (response: Response) => void;
  const latest = targets.map((target) => ({
    ...target,
    revision: 3,
    values: { ...target.values, caption: "External update" },
  }));
  const fetcher = vi.fn((url: string) =>
    url.endsWith("/edit")
      ? new Promise<Response>((resolve) => (finish = resolve))
      : Promise.resolve(response({ assets: [asset], targets: latest })),
  );
  vi.stubGlobal("fetch", fetcher);
  const editing = editor.edit({ asset_ids: ["asset"] }, { caption: "My edit" }, false, vi.fn());
  await vi.waitFor(() => expect(finish).toBeDefined());
  await editor.invalidate({ ids: new Set(["asset"]), idFree: false });
  finish(
    response({
      receipt_id: "edited",
      before: targets,
      after: targets.map((target) => ({
        ...target,
        revision: 2,
        values: { ...target.values, caption: "My edit" },
      })),
    }),
  );
  await editing;
  await vi.waitFor(() => expect(editor.value("caption")).toBe("External update"));
  expect(editor.targets.map((target) => target.revision)).toEqual([3, 3]);
  editor.dispose();
});
