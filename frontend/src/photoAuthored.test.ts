import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, render, screen, fireEvent, within } from "@testing-library/svelte";
import userEvent from "@testing-library/user-event";
import { tick } from "svelte";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { PhotoAuthoredEditor, PhotoUndoState } from "./photoAuthored.svelte.js";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
import { photoPreparation, preparedPhoto } from "./photo-test-fixtures.js";
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
function setupPhotoUI() {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  Element.prototype.scrollIntoView = vi.fn();
}
function ui(editor: PhotoAuthoredEditor, onedit = vi.fn(), onresponse = vi.fn()) {
  setupPhotoUI();
  return render(PhotoInspector, { editor, onedit, onresponse });
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
    let finish!: (response: Response) => void;
    let preparing: ReturnType<typeof editor.prepare> | undefined;
    if (kind === "write") {
      editor.busy = true;
      await editor.edit({}, { rating: 5 }, false, vi.fn());
      expect(editor.notice).toBe(
        "An edit is in progress. Try the shortcut again when it finishes.",
      );
    } else if (kind === "loading") {
      vi.stubGlobal(
        "fetch",
        vi.fn(() => new Promise<Response>((resolve) => (finish = resolve))),
      );
      preparing = editor.prepare({ asset_ids: ["asset"] });
    } else {
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
    if (kind === "draft") editor.clearDraft();
    if (preparing) {
      finish(response({ assets: [asset], targets }));
      await preparing;
      expect(editor.notice, "the loading warning clears once preparation settles").toBe("");
    }
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
    const before = targets.map((target) => ({ ...target, revision: 2, node_revision: 2 }));
    first.targets = structuredClone(before);
    const rated = before.map((target) => ({
      ...target,
      revision: 3,
      node_revision: 3,
      values: { ...target.values, rating: 5 },
    }));
    const picked = rated.map((target) => ({
      ...target,
      revision: 4,
      node_revision: 4,
      values: { ...target.values, flag: "pick" as const },
    }));
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response({ receipt_id: "first", before, after: rated }))
      .mockResolvedValueOnce(response({ receipt_id: "latest", before: rated, after: picked }));
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
      const current = structuredClone(picked);
      current[0].revision++;
      current[0].node_revision++;
      current[0].values.rating = 2;
      fetcher.mockImplementation(async (url: string) =>
        url.endsWith("/prepare")
          ? response(photoPreparation(current))
          : response({ detail: "Changed elsewhere", code: "stale_revision" }, 412),
      );
      first.dispose();
      const next = new PhotoAuthoredEditor("session", vi.fn(), state);
      await next.undo(refresh);
      expect(next.receiptID).toBe("latest");
      expect(next.error).toBe("This photo changed. This edit can no longer be undone.");
      await next.prepare({ asset_ids: [asset.id] });
      await next.undo(refresh);
      expect(fetcher.mock.calls[4][0]).toContain("/latest/undo");
      expect(next.error).toBe("This photo changed. This edit can no longer be undone.");
      expect(next.targets).toEqual(current);
      expect(refresh).not.toHaveBeenCalled();
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
    finish({
      assets: [asset],
      targets: structuredClone(targets).map((target) => ({ ...target, tags: ["deleted"] })),
    });
    await preparation;
    if (order === "preparation first") await refreshCatalog();
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

it("rereads a staged keyword renamed off the catalog page", async () => {
  const editor = editorWithTargets();
  const tag = (name: string) => ({ id: "staged", name, revision: 1, assignment_count: 0 }) as Tag;
  const other = { id: "other", name: "Other" } as Tag;
  editor.catalog([tag("Travel"), other]);
  editor.stage({}, [{ tag_id: "staged", assign: true }]);
  // The rename moved the keyword past the refreshed catalog page.
  editor.catalog([other]);
  expect(editor.tags.map((value) => value.name)).toEqual(["Other", "Travel"]);
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => response(tag("Voyages"))),
  );
  await editor.refreshAssignedTags();
  expect(editor.tags.map((value) => value.name)).toEqual(["Other", "Voyages"]);
  expect(editor.keywords).toEqual([{ tag_id: "staged", assign: true }]);
  editor.dispose();
});

it("waits for every keyword read before a refresh pass fails", async () => {
  const editor = editorWithTargets();
  editor.targets = [preparedPhoto(1, { tags: ["down", "slow"] })];
  let finish!: () => void;
  const fetcher = vi.fn((url: string) =>
    url.endsWith("/tags/down")
      ? Promise.resolve(response({ detail: "Down" }, 503))
      : new Promise<Response>(
          (resolve) =>
            (finish = () =>
              resolve(response({ id: "slow", name: "Slow", revision: 1, assignment_count: 1 }))),
        ),
  );
  vi.stubGlobal("fetch", fetcher);
  const slowReads = () => fetcher.mock.calls.filter(([url]) => url.endsWith("/tags/slow")).length;
  let settled = false;
  const refreshing = editor.refreshAssignedTags().finally(() => (settled = true));
  const failure = expect(refreshing).rejects.toThrow("Down");
  await vi.waitFor(() => expect(finish).toBeDefined());
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(settled).toBe(false);
  finish();
  await failure;
  expect(slowReads()).toBe(1);
  editor.dispose();
});

it("retries a live keyword read outside the catalog before consuming the queued change", async () => {
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
    await expect(editor.invalidate()).rejects.toThrow("Keywords unavailable");
    expect(editor.tags.map((tag) => tag.id)).toEqual(["catalog"]);
    // The watcher retries the pass after a failed read.
    await editor.invalidate();
    expect(editor.tags).toContainEqual(keyword);
    expect(reads).toBe(2);
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
    const refreshing = editor.invalidate();
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

// The first stack read can be in flight with nothing else prepared; the change must still reread it.
it("rereads a stack still preparing when a live change arrives, so a decision carries its new revisions", async () => {
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  const stack = (revision: number) =>
    structuredClone(targets).map((target) => ({ ...target, revision }));
  let finish!: (response: Response) => void;
  const fetcher = vi
    .fn()
    .mockImplementationOnce(() => new Promise<Response>((resolve) => (finish = resolve)))
    .mockResolvedValue(response({ assets: [asset], targets: stack(2) }));
  vi.stubGlobal("fetch", fetcher);
  const preparing = editor.prepareStack(["asset"], false);
  await vi.waitFor(() => expect(finish).toBeDefined());
  const refreshing = editor.invalidate();
  finish(response({ assets: [asset], targets: stack(1) }));
  await preparing;
  await refreshing;
  expect(editor.stackTargets.map((target) => target.revision)).toEqual([2, 2]);
  const decide = vi.spyOn(api, "decidePhotoStack").mockResolvedValue({
    receipt_id: "stack-receipt",
    before: [],
    after: [],
  });
  await editor.decideStack(
    { asset_ids: ["asset"], decision: "all", current_id: "asset", burst_gap_seconds: 3 },
    vi.fn(),
    { observed: [] },
  );
  expect(decide.mock.calls[0]![0].targets).toMatchObject([{ revision: 2 }, { revision: 2 }]);
  editor.dispose();
});

it("lets a waiting live change finish when the editor is disposed during a stack read", async () => {
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  vi.stubGlobal(
    "fetch",
    vi.fn(() => new Promise<Response>(() => {})),
  );
  void editor.prepareStack(["asset"], false);
  await vi.waitFor(() => expect(editor.stackLoading).toBe(true));
  const waiting = editor.invalidate();
  editor.dispose();
  await waiting;
  expect(editor.stackLoading).toBe(false);
});

// A live update refreshes the prepared stack in the background: the old members stay usable
// until the new read lands, and a failed read keeps them and leaves the change pending.
it("keeps the prepared stack usable while a live update refreshes it", async () => {
  const editor = editorWithTargets();
  const sibling = preparedPhoto(3, { asset_id: "other", file_id: "other-file" });
  const stack = () => [...structuredClone(targets), sibling];
  const revisions = () => editor.stackTargets.map((target) => target.revision);
  const fetcher = vi.fn().mockResolvedValue(response({ assets: [asset], targets: stack() }));
  vi.stubGlobal("fetch", fetcher);
  await editor.prepareStack(["asset", "other"], false);
  expect(revisions()).toEqual([1, 1, 1]);
  let finish!: (response: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise<Response>((resolve) => (finish = resolve)));
  const refreshing = editor.invalidate();
  await vi.waitFor(() => expect(finish).toBeDefined());
  expect([editor.stackLoading, editor.loading, editor.busy]).toEqual([false, false, false]);
  expect(editor.needsStack(["asset", "other"], false)).toBe(false);
  expect(revisions()).toEqual([1, 1, 1]);
  const latest = stack().map((target) => ({ ...target, revision: 7 }));
  fetcher.mockResolvedValue(response({ assets: [asset], targets: latest }));
  finish(response({ assets: [asset], targets: latest.slice(0, 2) }));
  await refreshing;
  expect(editor.stackLoading).toBe(false);
  expect(revisions()).toEqual([7, 7, 7]);
  fetcher.mockResolvedValueOnce(response({ assets: [asset], targets: latest.slice(0, 2) }));
  fetcher.mockResolvedValueOnce(response({ detail: "Metadata unavailable" }, 503));
  await expect(editor.invalidate()).rejects.toThrow("Metadata unavailable");
  expect(editor.stackLoading).toBe(false);
  expect(revisions()).toEqual([7, 7, 7]);
  editor.dispose();
});

// A quiet read in flight when a write starts is dropped: it must neither hold the write up
// behind Hidden access nor land on revisions the write has already moved on.
function editAfterQuietRead(editor: PhotoAuthoredEditor, stack = false) {
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
  if (stack) void editor.prepareStack(["asset"], false, false, true);
  else void editor.reload(false, true);
  const edit = editor.edit({ asset_ids: ["asset"] }, { rating: 5 }, false, vi.fn());
  return { edit, fetcher, answer: () => answer() };
}

it.each([
  [false, false],
  [true, false],
  [false, true],
  [true, true],
])(
  "drops a pending quiet metadata read when an edit starts (Hidden access: %s, stack: %s)",
  async (hidden, stack) => {
    const editor = new PhotoAuthoredEditor(
      "session",
      vi.fn(),
      undefined,
      hidden ? new HiddenAccess("session", vi.fn()) : undefined,
    );
    editor.targets = structuredClone(targets);
    editor.asset = structuredClone(asset);
    const { edit, fetcher, answer } = editAfterQuietRead(editor, stack);
    await vi.waitFor(() =>
      expect(fetcher.mock.calls.some(([url]) => String(url).endsWith("/edit"))).toBe(true),
    );
    await edit;
    answer();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(editor.targets.map((target) => target.revision)).toEqual([7, 7]);
    expect(editor.targets.map((target) => target.values.rating)).toEqual([5, 5]);
    expect(editor.stackTargets).toEqual([]);
  },
);

it("refuses a queued Hidden edit once a live refresh drops one of its photos", async () => {
  const access = new HiddenAccess("session", vi.fn());
  const editor = new PhotoAuthoredEditor("session", vi.fn(), undefined, access);
  editor.targets = ["a", "b"].map((id, index) =>
    preparedPhoto(index + 1, { asset_id: id, file_id: `${id}-file` }),
  );
  const fetcher = vi.fn();
  vi.stubGlobal("fetch", fetcher);
  let release!: () => void;
  void access.run(() => new Promise<void>((resolve) => (release = resolve)));
  await vi.waitFor(() => expect(release).toBeDefined());
  const refresh = vi.fn();
  const edit = editor.edit({ asset_ids: ["a", "b"] }, { rating: 5 }, true, refresh);
  expect(editor.busy).toBe(true);
  editor.revalidate(new Set(["a"]));
  expect(editor.conflict).toBe(true);
  release();
  await edit;
  expect(fetcher).not.toHaveBeenCalled();
  expect(refresh).not.toHaveBeenCalled();
  expect(editor.conflict).toBe(true);
  expect(editor.patch).toEqual({ rating: 5 });
  expect(editor.notice).toBe("Reload metadata before editing.");
  editor.dispose();
});

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
  await editor.invalidate();
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

function withXMP(editor: PhotoAuthoredEditor, xmp: api.PhotoXMPState) {
  editor.targets[0]!.xmp = xmp;
  return editor;
}
const conflict: api.PhotoXMPState = {
  state: "conflict",
  conflict_id: "40000000-0000-4000-8000-000000000009",
  rows: [{ field: "Rating", catalog: "4", sidecar: "1" }],
};

it.each(["resolution", "background"])(
  "refreshes later-page keyword records after XMP %s",
  async (mode) => {
    const before = { ...targets[0]!, revision: 2, node_revision: 2 };
    const editor = withXMP(editorWithTargets(), structuredClone(conflict));
    editor.targets[0] = { ...before, xmp: structuredClone(conflict) };
    const catalog = Array.from({ length: 1001 }, (_, i) => ({
      id: `tag-${i}`,
      name: `Keyword ${i}`,
      revision: 1,
      assignment_count: 0,
    })) as Tag[];
    const fresh = { ...catalog[1000]!, revision: 2, assignment_count: 1 };
    const imported = {
      ...before,
      revision: 3,
      node_revision: 4,
      values: { ...before.values, rating: 1 },
      tags: [fresh.id],
      xmp: { state: "synced" as const, import_receipt_id: "import" },
    };
    const receipt = {
      receipt_id: "import",
      before: [{ file_id: "raw", node_id: 1, revision: before.revision, values: before.values }],
      after: [{ file_id: "raw", node_id: 1, revision: imported.revision, values: imported.values }],
      sidecar: {
        node_id: 3,
        file_id: "sidecar",
        version_id: "40000000-0000-4000-8000-000000000003",
      },
      keywords: [{ node_id: 1, tag_id: fresh.id, before: false, after: true }],
      keyword_nodes: [{ node_id: 1, after_revision: 4 }],
    };
    editor.catalog(catalog);
    editor.loadTags = async () => editor.catalog(catalog.slice(0, 1000));
    const fetcher = vi.fn(async (url: string) => {
      if (url.endsWith("/xmp")) return response(receipt);
      if (url.endsWith(`/tags/${fresh.id}`)) return response(fresh);
      if (url.endsWith("/photos/authored/prepare"))
        return response(
          photoPreparation([imported, { ...targets[1]!, revision: 2, node_revision: 2 }]),
        );
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetcher);
    const workspace = vi.fn(async () => {});
    editor.onXMPChanged = workspace;
    ui(editor);
    if (mode === "resolution") {
      await editor.resolveXMP("raw", "sidecar");
    } else {
      editor.targets[0]!.xmp = { state: "pending" };
      await editor.refreshXMP();
    }
    await vi.waitFor(() =>
      expect(workspace).toHaveBeenCalledWith(mode === "resolution" ? receipt : undefined, [fresh]),
    );
    expect(editor.tags.find((tag) => tag.id === fresh.id)).toEqual(fresh);
    await fireEvent.click(screen.getByRole("combobox", { name: /Keyword:/ }));
    expect(
      within(screen.getByRole("option", { name: /Keyword 1000/ })).getByText("1", { exact: true }),
    ).toBeTruthy();
    editor.dispose();
  },
);

it("keeps the inspected conflict and polling when preparing its stack", async () => {
  vi.useFakeTimers();
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  const target = { ...structuredClone(targets[0]!), xmp: structuredClone(conflict) };
  const fetcher = vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/xmp")) return response({});
    const request = JSON.parse(options!.body as string);
    return response(
      request.stack
        ? photoPreparation([...structuredClone(targets), preparedPhoto(3, { asset_id: "other" })])
        : photoPreparation([target]),
    );
  });
  vi.stubGlobal("fetch", fetcher);
  try {
    await editor.prepare({ asset_ids: ["asset"] });
    await editor.prepareStack(["asset", "other"], false);
    expect(editor.targets[0]!.xmp?.state).toBe("conflict");
    await vi.advanceTimersByTimeAsync(2000);
    expect(fetcher).toHaveBeenCalledTimes(3);
  } finally {
    editor.dispose();
    vi.useRealTimers();
  }
});

it("shows conflicting properties and resolves the recorded conflict", async () => {
  const before = { ...targets[0]!, revision: 2, node_revision: 2 };
  const editor = withXMP(editorWithTargets(), structuredClone(conflict));
  editor.targets[0] = { ...before, xmp: structuredClone(conflict) };
  editor.fileID = "raw";
  const imported = {
    ...before,
    revision: 3,
    node_revision: before.node_revision + 1,
    values: { ...before.values, rating: 1 },
    xmp: { state: "synced" as const, import_receipt_id: "import" },
  };
  const receipt = {
    receipt_id: "import",
    before: [{ file_id: "raw", node_id: 1, revision: before.revision, values: before.values }],
    after: [{ file_id: "raw", node_id: 1, revision: imported.revision, values: imported.values }],
    sidecar: { node_id: 3, file_id: "sidecar", version_id: "40000000-0000-4000-8000-000000000003" },
  };
  const fetcher = vi.fn<typeof fetch>(async (url) =>
    String(url).endsWith("/xmp")
      ? response(receipt)
      : response(photoPreparation([imported, { ...targets[1]!, revision: 2, node_revision: 2 }])),
  );
  vi.stubGlobal("fetch", fetcher);
  ui(editor);
  const panel = screen.getByRole("region", { name: "XMP for capture.raw" });
  expect(within(panel).getByText("XMP: Conflict")).toBeTruthy();
  expect(within(panel).getByRole("status").textContent).toContain("XMP: Conflict");
  expect(within(panel).getByRole("row", { name: "Rating 4 1" })).toBeTruthy();
  await fireEvent.click(within(panel).getByRole("button", { name: "Use Sidecar" }));
  await vi.waitFor(() => expect(editor.targets[0]!.xmp?.state).toBe("synced"));
  expect(String(fetcher.mock.calls[0]![0])).toContain("/photos/authored/raw/xmp");
  expect(JSON.parse(fetcher.mock.calls[0]![1]!.body as string)).toEqual({
    conflict_id: conflict.conflict_id,
    choice: "sidecar",
  });
  expect(editor.targets[0]!.xmp?.import_receipt_id).toBe("import");
  expect(editor.targets[0]!.values.rating).toBe(1);
  expect(screen.getByRole("combobox", { name: /Rating:/ }).textContent).toContain("1 star");
  expect(editor.asset!.files.find((file) => file.id === "raw")!.rating).toBe(1);
  editor.dispose();
});

it("keeps invalid conflict values visible and disables choices", async () => {
  const editor = withXMP(editorWithTargets(), {
    ...structuredClone(conflict),
    state: "invalid",
    reason: "Packet unreadable",
  });
  editor.targets[0]!.revision = 2;
  editor.targets[0]!.node_revision = 2;
  const fetcher = vi.fn<typeof fetch>();
  vi.stubGlobal("fetch", fetcher);
  ui(editor);
  const panel = screen.getByRole("region", { name: "XMP for capture.raw" });
  expect(within(panel).getByText("Packet unreadable")).toBeTruthy();
  expect(within(panel).getByRole("row", { name: "Rating 4 1" })).toBeTruthy();
  for (const name of ["Use Catalog", "Use Sidecar"]) {
    const button = within(panel).getByRole("button", { name }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    await fireEvent.click(button);
  }
  expect(fetcher).not.toHaveBeenCalled();
  editor.dispose();
});

it("asks for a refresh when the conflict changed before the choice", async () => {
  const editor = withXMP(editorWithTargets(), structuredClone(conflict));
  editor.targets[0]!.revision = 2;
  editor.targets[0]!.node_revision = 2;
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response({ detail: "stale" }, 412)));
  await editor.resolveXMP("raw", "catalog");
  expect(editor.error).toBe("The photo or sidecar changed. Reopen the photo and choose again.");
  editor.dispose();
});

it("lets the stack prepare again when a write that interrupted it fails", async () => {
  const editor = withXMP(editorWithTargets(), structuredClone(conflict));
  let finish!: (value: api.PhotoAuthoredPreparation) => void;
  vi.spyOn(api, "preparePhotoAuthored").mockImplementation(
    () => new Promise((resolve) => (finish = resolve)),
  );
  vi.spyOn(api, "resolvePhotoXMP").mockRejectedValue(new Error("offline"));
  const preparing = editor.prepareStack(["asset", "other"], false);
  await vi.waitFor(() => expect(editor.stackLoading).toBe(true));
  await editor.resolveXMP("raw", "catalog");
  expect(editor.error).toBe("offline");
  expect(editor.stackLoading).toBe(false);
  expect(editor.needsStack(["asset", "other"], false)).toBe(true);
  finish(photoPreparation(structuredClone(targets)));
  await preparing;
  editor.dispose();
});

it("invalidates old XMP conflict details after an authored edit", async () => {
  vi.useFakeTimers();
  const before = { ...targets[0]!, revision: 2, node_revision: 2 };
  const editor = withXMP(editorWithTargets(), {
    ...structuredClone(conflict),
    import_receipt_id: "old-import",
  });
  editor.targets[0]!.revision = before.revision;
  editor.targets[0]!.node_revision = before.node_revision;
  try {
    editor.fileID = "raw";
    vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
      receipt_id: "new-edit",
      before: [{ file_id: "raw", node_id: 1, revision: before.revision, values: before.values }],
      after: [{ file_id: "raw", node_id: 1, revision: 3, values: { ...before.values, rating: 5 } }],
    });
    ui(editor);
    const panel = screen.getByRole("region", { name: "XMP for capture.raw" });
    expect(within(panel).getByRole("row", { name: "Rating 4 1" })).toBeTruthy();
    expect(within(panel).getByRole("button", { name: "Undo sidecar import" })).toBeTruthy();
    await editor.edit(
      {},
      { rating: 5 },
      false,
      vi.fn(async () => {}),
    );
    await vi.waitFor(() => expect(within(panel).getByText("XMP: Pending")).toBeTruthy());
    const pending = editor.targets[0]!.xmp!;
    expect(pending.conflict_id).toBeUndefined();
    expect(pending.rows ?? []).toHaveLength(0);
    expect(pending.import_receipt_id).toBeUndefined();
    expect(within(panel).queryByRole("row", { name: "Rating 4 1" })).toBeNull();
    for (const name of ["Use Catalog", "Use Sidecar", "Undo sidecar import"])
      expect(within(panel).queryByRole("button", { name })).toBeNull();
    const prepare = vi
      .spyOn(api, "preparePhotoAuthored")
      .mockResolvedValue(
        photoPreparation(editor.targets.map((target) => ({ ...target, xmp: { state: "synced" } }))),
      );
    await vi.advanceTimersByTimeAsync(2000);
    expect(editor.targets[0]!.xmp?.state).toBe("synced");
    expect(prepare).toHaveBeenCalledTimes(1);
  } finally {
    editor.dispose();
    vi.useRealTimers();
  }
});

it.each([
  { from: "synced", to: "synced" },
  { from: "pending", to: "invalid" },
] as const)("notifies the workspace once when XMP goes from $from to $to", async ({ from, to }) => {
  vi.useFakeTimers();
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  const target = {
    ...structuredClone(targets[0]!),
    revision: 2,
    node_revision: 2,
    xmp: { state: from } as api.PhotoXMPState,
  };
  // A synced target changes by importing an outside edit; the others settle with no file revision change.
  const settled =
    from === to
      ? {
          ...target,
          revision: target.revision + 1,
          node_revision: target.node_revision + 1,
          values: { ...target.values, rating: 5 },
          xmp: { state: to, import_receipt_id: "40000000-0000-4000-8000-000000000008" },
        }
      : { ...target, xmp: { state: to } as api.PhotoXMPState };
  const prepare = vi
    .spyOn(api, "preparePhotoAuthored")
    .mockResolvedValueOnce(photoPreparation([target]))
    .mockResolvedValue(photoPreparation([settled]));
  try {
    await editor.prepare({ asset_ids: ["asset"] });
    ui(editor);
    const changed = vi.fn(async () => {});
    editor.onXMPChanged = changed;
    await vi.advanceTimersByTimeAsync(2000);
    await tick();
    expect(prepare).toHaveBeenCalledTimes(2);
    expect(editor.targets[0]!.xmp).toEqual(settled.xmp);
    expect(editor.targets[0]!.values.rating).toBe(settled.values.rating);
    expect(changed).toHaveBeenCalledTimes(1);
    if (from === to) {
      // Polling resumes at the first interval, and the same import does not notify again.
      await vi.advanceTimersByTimeAsync(2000);
      expect(prepare).toHaveBeenCalledTimes(3);
      expect(changed).toHaveBeenCalledTimes(1);
    }
  } finally {
    editor.dispose();
    vi.useRealTimers();
  }
});

it.each(["pending", "retry"])("backs off %s XMP polls until a batch", async (outcome) => {
  vi.useFakeTimers();
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  const target = {
    ...structuredClone(targets[0]!),
    revision: 2,
    node_revision: 2,
    xmp: { state: "pending" as const },
  };
  const prepared = photoPreparation([target]);
  const prepare = vi.spyOn(api, "preparePhotoAuthored").mockResolvedValue(prepared);
  try {
    await editor.prepare({ asset_ids: ["asset"] });
    if (outcome === "retry")
      prepare.mockRejectedValue(new APIError("Unavailable", 503, "unavailable"));
    for (const [attempt, delay] of [
      2000, 4000, 8000, 16000, 30000, 30000, 30000, 30000,
    ].entries()) {
      await vi.advanceTimersByTimeAsync(delay - 1);
      expect(prepare).toHaveBeenCalledTimes(attempt + 1);
      await vi.advanceTimersByTimeAsync(1);
      expect(prepare).toHaveBeenCalledTimes(attempt + 2);
    }
    await vi.advanceTimersByTimeAsync(30000);
    expect(prepare).toHaveBeenCalledTimes(10);
    expect(editor.targets[0]!.xmp?.state).toBe("pending");
    prepare.mockResolvedValue(prepared);
    await editor.prepare({ asset_ids: ["asset"] });
    await vi.advanceTimersByTimeAsync(2000);
    expect(prepare).toHaveBeenCalledTimes(12);
    prepare.mockResolvedValue({ assets: [], targets: [preparedPhoto(1), preparedPhoto(2)] });
    await editor.prepare({ asset_ids: ["photo-1", "photo-2"] });
    await vi.advanceTimersByTimeAsync(120000);
    expect(prepare).toHaveBeenCalledTimes(13);
    prepare.mockResolvedValue(prepared);
    await editor.prepare({ asset_ids: ["asset"] });
    editor.dispose();
    await vi.advanceTimersByTimeAsync(3600000);
    expect(prepare).toHaveBeenCalledTimes(14);
  } finally {
    editor.dispose();
    vi.useRealTimers();
  }
});

it.each([401, 404])("stops XMP polling after a terminal %s response", async (status) => {
  vi.useFakeTimers();
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  const target = {
    ...structuredClone(targets[0]!),
    revision: 2,
    node_revision: 2,
    xmp: { state: "pending" as const },
  };
  const prepare = vi
    .spyOn(api, "preparePhotoAuthored")
    .mockResolvedValue(photoPreparation([target]));
  try {
    await editor.prepare({ asset_ids: ["asset"] });
    prepare.mockRejectedValue(new APIError("Unavailable", status, "unavailable"));
    await vi.advanceTimersByTimeAsync(3600000);
    expect(prepare).toHaveBeenCalledTimes(2);
  } finally {
    editor.dispose();
    vi.useRealTimers();
  }
});

it.each(["authored values", "membership", "XMP conflict"])(
  "preserves a draft when background refresh finds changed %s",
  async (changed) => {
    vi.useFakeTimers();
    const editor = new PhotoAuthoredEditor("session", vi.fn());
    const target = {
      ...structuredClone(targets[0]!),
      revision: 2,
      node_revision: 2,
      xmp: { state: "pending" as const },
    };
    const refreshed =
      changed === "authored values"
        ? [{ ...target, revision: 3, node_revision: 3, values: { ...target.values, rating: 1 } }]
        : changed === "membership"
          ? [
              { ...target, asset_revision: 2 },
              { ...structuredClone(targets[1]!), revision: 2, node_revision: 2, asset_revision: 2 },
            ]
          : [{ ...target, xmp: structuredClone(conflict) }];
    const refreshedPreparation = photoPreparation(refreshed);
    if (changed === "membership") refreshedPreparation.assets[0]!.revision = 2;
    const prepare = vi
      .spyOn(api, "preparePhotoAuthored")
      .mockResolvedValueOnce(photoPreparation([target]))
      .mockResolvedValue(refreshedPreparation);
    const edit = vi.spyOn(api, "editPhotoAuthored");
    try {
      await editor.prepare({ asset_ids: ["asset"] });
      ui(editor);
      editor.stage({ caption: "Harbor draft" });
      await vi.advanceTimersByTimeAsync(2000);
      expect(editor.patch).toEqual({ caption: "Harbor draft" });
      if (changed === "XMP conflict") {
        await tick();
        expect(prepare.mock.calls[1]?.[0].asset_ids).toEqual(["asset"]);
        expect(editor.conflict).toBe(false);
        expect(editor.targets[0]!.xmp?.state).toBe("conflict");
        expect(screen.getByRole("row", { name: "Rating 4 1" })).toBeTruthy();
        expect(editor.targets[0]!.values).toEqual(target.values);
        expect(editor.targets[0]!.asset_revision).toBe(1);
        expect(
          (screen.getByRole("textbox", { name: "Caption" }) as HTMLTextAreaElement).value,
        ).toBe("Harbor draft");
      } else {
        expect(editor.conflict).toBe(true);
        expect(editor.targets[0]!.values.rating).toBe(4);
        expect(editor.targets[0]!.revision).toBe(2);
        expect(editor.targets[0]!.node_revision).toBe(2);
        await editor.edit(
          {},
          {},
          false,
          vi.fn(async () => {}),
        );
        expect(edit).not.toHaveBeenCalled();
      }
    } finally {
      editor.dispose();
      vi.useRealTimers();
    }
  },
);

it("ignores an XMP refresh that predates the next authored edit", async () => {
  vi.useFakeTimers();
  const editor = new PhotoAuthoredEditor("session", vi.fn());
  const target = {
    ...structuredClone(targets[0]!),
    revision: 2,
    node_revision: 2,
    xmp: { state: "pending" as const },
  };
  let finish!: (value: api.PhotoAuthoredPreparation) => void;
  vi.spyOn(api, "preparePhotoAuthored")
    .mockResolvedValueOnce(photoPreparation([target]))
    .mockImplementationOnce(() => new Promise((resolve) => (finish = resolve)));
  vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
    receipt_id: "new-edit",
    before: [{ file_id: "raw", node_id: 1, revision: target.revision, values: target.values }],
    after: [{ file_id: "raw", node_id: 1, revision: 3, values: { ...target.values, rating: 5 } }],
  });
  try {
    await editor.prepare({ asset_ids: ["asset"] });
    await vi.advanceTimersByTimeAsync(2000);
    await editor.edit(
      {},
      { rating: 5 },
      false,
      vi.fn(async () => {}),
    );
    finish(photoPreparation([{ ...target, xmp: structuredClone(conflict) }]));
    await tick();
    expect(editor.targets[0]!.xmp).toEqual({ state: "pending" });
    expect(editor.targets[0]!.revision).toBe(3);
    expect(editor.targets[0]!.values.rating).toBe(5);
    expect(editor.conflict).toBe(false);
  } finally {
    editor.dispose();
    vi.useRealTimers();
  }
});

it.each(["originals without a sidecar or decisions", "a batch of assets"])(
  "omits XMP status for %s",
  (scenario) => {
    const editor = editorWithTargets();
    if (scenario === "a batch of assets") {
      editor.asset = undefined;
      editor.targets = Array.from({ length: 50 }, (_, index) =>
        preparedPhoto(index + 1, {
          asset_id: `asset-${index}`,
          file_id: `file-${index}`,
          file_name: `capture-${index}.raw`,
        }),
      );
    } else
      editor.targets = editor.targets.map((target) => ({
        ...target,
        values: preparedPhoto(1).values,
        xmp: { state: "none" },
      }));
    ui(editor);
    expect(screen.queryAllByRole("region", { name: /^XMP for / })).toHaveLength(0);
    editor.dispose();
  },
);

it("enables Accept selected only while a field is checked", async () => {
  const editor = editorWithTargets();
  editor.suggestions = [
    photoSuggestion({ value: { caption: "A harbor.", tags: ["coast"], visible_text: "QUAYSIDE" } }),
  ];
  ui(editor);
  const accept = screen.getByRole("button", { name: "Accept selected" }) as HTMLButtonElement;
  expect(accept.disabled).toBe(true);
  await fireEvent.click(screen.getByRole("checkbox", { name: "A harbor." }));
  expect(accept.disabled).toBe(false);
  await fireEvent.click(screen.getByRole("checkbox", { name: "A harbor." }));
  expect(accept.disabled).toBe(true);
  await fireEvent.click(screen.getByRole("checkbox", { name: "coast" }));
  expect(accept.disabled).toBe(false);
  editor.dispose();
});

it("hides the suggestions while several photos are selected", async () => {
  const editor = editorWithTargets();
  editor.suggestions = [
    photoSuggestion({ value: { caption: "A harbor.", tags: ["coast"], visible_text: "QUAYSIDE" } }),
  ];
  const view = ui(editor);
  await view.rerender({ actionDisabled: false, count: 2 });
  expect(screen.queryByRole("region", { name: "Suggested photo metadata" })).toBeNull();
  editor.dispose();
});

it.each(["busy", "loading", "conflict", "dirty", "action disabled"])(
  "disables suggestion choices while %s",
  async (state) => {
    const editor = editorWithTargets();
    editor.suggestions = [
      photoSuggestion({
        value: { caption: "A harbor.", tags: ["coast"], visible_text: "QUAYSIDE" },
      }),
    ];
    const view = ui(editor);
    const accept = screen.getByRole("button", { name: "Accept selected" }) as HTMLButtonElement;
    await fireEvent.click(screen.getByRole("checkbox", { name: "A harbor." }));
    if (state === "busy") editor.busy = true;
    if (state === "loading") editor.loading = true;
    if (state === "conflict") editor.conflict = true;
    if (state === "dirty") editor.stage({ rating: 5 });
    await view.rerender({ actionDisabled: state === "action disabled", count: 1 });
    const section = within(screen.getByRole("region", { name: "Suggested photo metadata" }));
    expect(accept.disabled).toBe(true);
    expect((section.getByRole("button", { name: "Dismiss" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    for (const checkbox of section.getAllByRole("checkbox"))
      expect((checkbox as HTMLInputElement).disabled).toBe(true);
    editor.dispose();
  },
);

it.each(["raw", "jpeg"])("identifies the suggested %s member beside a long filename", (member) => {
  const editor = editorWithTargets();
  editor.asset!.display_file_id = member;
  const target = editor.targets.find((target) => target.file_id === member)!;
  target.file_name = `Synthetic-coastal-harbor-summer-collection-evening-light-study.${member === "raw" ? "cr2" : "jpg"}`;
  editor.suggestions = [
    photoSuggestion({
      target: member,
      value: { caption: "A harbor.", tags: [], visible_text: "" },
    }),
  ];
  ui(editor);
  const suggestions = within(screen.getByRole("region", { name: "Suggested photo metadata" }));
  expect(suggestions.getByText(member === "raw" ? "RAW ·" : "IMAGE ·")).toBeTruthy();
  expect(suggestions.getByText(target.file_name).parentElement?.title).toBe(target.file_name);
  editor.dispose();
});

it.each(["", "Saved caption"])(
  "shows caption replacement for the suggested member with %j",
  (caption) => {
    const editor = editorWithTargets();
    editor.targets[0]!.values.caption = "Other member caption";
    editor.targets[1]!.values.caption = caption;
    editor.suggestions = [
      photoSuggestion({ value: { caption: "A harbor.", tags: [], visible_text: "" } }),
    ];
    ui(editor);
    const checkbox = screen.getByRole("checkbox", { name: "A harbor." });
    const note = screen.queryByText("Replaces current caption");
    expect(!!note).toBe(!!caption);
    expect(checkbox.getAttribute("aria-describedby")).toBe(note?.id ?? null);
    editor.dispose();
  },
);

it.each([true, false])(
  "responds with current revisions after Keep all, accept=%s",
  async (accept) => {
    const editor = editorWithTargets();
    editor.suggestions = [
      photoSuggestion({ value: { caption: "A harbor.", tags: [], visible_text: "" } }),
    ];
    const stackTargets = [
      ...targets,
      preparedPhoto(3, { asset_id: "other", file_id: "other-file" }),
    ]
      .map((target) => ({
        ...target,
        values: { ...target.values, confirmed_fields: target.values.rating ? 1 : 0 },
      }))
      .sort((a, b) => a.file_id.localeCompare(b.file_id));
    vi.spyOn(api, "preparePhotoAuthored").mockResolvedValue({
      assets: [asset],
      targets: stackTargets,
      suggestions: [],
    });
    await editor.prepareStack(["asset", "other"], false);
    vi.spyOn(api, "decidePhotoStack").mockResolvedValue({
      receipt_id: "stack-receipt",
      before: stackTargets.map(({ file_id, node_id, revision, values }) => ({
        file_id,
        node_id,
        revision,
        values,
      })),
      after: stackTargets.map(({ file_id, node_id, values }) => ({
        file_id,
        node_id,
        revision: 2,
        values: { ...values, flag: "pick", confirmed_fields: values.confirmed_fields | 2 },
      })),
    });
    const respond = vi.spyOn(api, "respondPhotoSuggestion").mockResolvedValue(photoSuggestion({}));
    expect(
      await editor.decideStack(
        {
          asset_ids: ["asset", "other"],
          decision: "all",
          current_id: "asset",
          burst_gap_seconds: 3,
        },
        vi.fn(),
        { observed: [] },
      ),
    ).toBe(true);
    await editor.respondSuggestion(editor.suggestions[0]!, accept, [], !accept);
    expect(respond).toHaveBeenCalledWith(
      "suggestion",
      expect.objectContaining({
        response: accept ? "accepted" : "dismissed",
        asset_id: "asset",
        asset_revision: 1,
        source_revision: 2,
        node_revision: 2,
      }),
      expect.anything(),
    );
    editor.dispose();
  },
);

it.each(["rating", "rating and keywords"])(
  "accepts a suggestion after an authored %s edit",
  async (field) => {
    const editor = editorWithTargets();
    editor.fileID = "jpeg";
    editor.suggestions = [
      photoSuggestion({ value: { caption: "A harbor.", tags: [], visible_text: "" } }),
    ];
    const nodeRevision = field === "rating and keywords" ? 3 : 2;
    let accepted = false;
    const fetcher = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url.endsWith("/photos/authored/edit"))
        return response({
          receipt_id: "metadata-receipt",
          before: [],
          after: [{ file_id: "jpeg", revision: 2, values: { ...targets[1]!.values, rating: 5 } }],
          keywords:
            field === "rating and keywords" ? [{ node_id: 2, tag_id: "coast", after: true }] : [],
          keyword_nodes: field === "rating and keywords" ? [{ node_id: 2, after_revision: 3 }] : [],
        });
      if (url.endsWith("/tags/coast")) return response({ id: "coast", name: "Coast" });
      if (url.endsWith("/photos/suggestions/suggestion/response")) {
        const request = JSON.parse(String(init?.body));
        if (request.source_revision !== 2 || request.node_revision !== nodeRevision)
          return response({ detail: "Photo changed" }, 409);
        accepted = true;
        return response({
          ...editor.suggestions[0],
          value: { caption: "", tags: [], visible_text: "" },
          authored_receipt: { receipt_id: "caption-receipt", before: [], after: [], keywords: [] },
        });
      }
      if (url.endsWith("/photos/authored/prepare"))
        return preparedSuggestions(
          accepted
            ? []
            : [
                photoSuggestion({
                  value: { caption: "A harbor.", tags: [], visible_text: "" },
                  source_revision: 2,
                  node_revision: nodeRevision,
                }),
              ],
        );
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetcher);
    await editor.edit({}, { rating: 5 }, false, vi.fn(), {
      keywords: field === "rating and keywords" ? [{ tag_id: "coast", assign: true }] : [],
    });
    expect(editor.error).toBe("");
    await editor.respondSuggestion(editor.suggestions[0]!, true);
    expect(editor.suggestionsError).toBe("");
    expect(editor.receiptID).toBe("caption-receipt");
    expect(editor.undoState.summary).toBe("Accepted caption");
    fetcher.mockImplementationOnce(async (url: string) => {
      expect(url).toContain("/caption-receipt/undo");
      return response({
        receipt_id: "inverse",
        after: [{ file_id: "jpeg", revision: 4, values: { ...targets[1]!.values, rating: 5 } }],
      });
    });
    const refresh = vi.fn();
    await editor.undo(refresh);
    expect(editor.error).toBe("");
    expect(editor.receiptID).toBe("");
    expect(refresh).toHaveBeenCalledOnce();
    expect(editor.targets.find((target) => target.file_id === "jpeg")!.values.rating).toBe(5);
    expect(editor.suggestions).toEqual([]);
    editor.dispose();
  },
);

it("keeps the latest authored Undo when dismissing a partly accepted suggestion", async () => {
  const editor = editorWithTargets();
  editor.fileID = "jpeg";
  const suggestion = photoSuggestion({
    value: { caption: "A harbor.", tags: ["coast"], visible_text: "QUAYSIDE" },
  });
  const accepted = {
    ...suggestion,
    value: { caption: "", tags: [], visible_text: "QUAYSIDE" },
    authored_receipt: { receipt_id: "older-caption-receipt", before: [], after: [], keywords: [] },
  };
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(response(accepted))
      .mockResolvedValueOnce(preparedSuggestions([accepted])),
  );
  await editor.respondSuggestion(suggestion, true, ["coast"]);
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(
        response({
          receipt_id: "latest-rating-receipt",
          before: [],
          after: [{ file_id: "jpeg", revision: 2, values: { ...targets[1]!.values, rating: 5 } }],
        }),
      )
      .mockResolvedValueOnce(preparedSuggestions([accepted])),
  );
  await editor.edit({}, { rating: 5 }, false, vi.fn());
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(
        response({ ...suggestion, value: { caption: "", tags: [], visible_text: "" } }),
      )
      .mockResolvedValueOnce(preparedSuggestions([])),
  );
  await editor.respondSuggestion(suggestion, false, [], true);
  expect(editor.receiptID).toBe("latest-rating-receipt");
  expect(editor.undoState.summary).toBe("1 file updated: rating");
  editor.dispose();
});

it.each([
  { name: "tags", value: { caption: "", tags: ["coast"], visible_text: "" } },
  { name: "visible text", value: { caption: "", tags: [], visible_text: "QUAYSIDE" } },
])("shows a pending row with content: $name", ({ value }) => {
  const editor = editorWithTargets();
  editor.suggestions = [photoSuggestion({ value })];
  ui(editor);
  const suggestion = within(screen.getByRole("region", { name: "Suggested photo metadata" }));
  expect((suggestion.getByRole("button", { name: "Dismiss" }) as HTMLButtonElement).disabled).toBe(
    false,
  );
  if (value.tags.length) {
    expect((suggestion.getByRole("checkbox", { name: "coast" }) as HTMLInputElement).disabled).toBe(
      false,
    );
    expect(
      (suggestion.getByRole("button", { name: "Accept selected" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  } else {
    expect(suggestion.getByText("QUAYSIDE")).toBeTruthy();
    expect(suggestion.queryByRole("checkbox")).toBeNull();
    expect(suggestion.queryByRole("button", { name: "Accept selected" })).toBeNull();
  }
  editor.dispose();
});

it("reloads reviewed photo revisions after a stale suggestion response", async () => {
  const editor = editorWithTargets();
  editor.fileID = "jpeg";
  const suggestion = photoSuggestion({
    value: { caption: "A harbor.", tags: [], visible_text: "" },
  });
  editor.suggestions = [suggestion];
  const refreshed = targets.map((target) => ({ ...target, revision: 9, node_revision: 10 }));
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(response({ detail: "photo revision changed" }, 409))
    .mockResolvedValueOnce(
      preparedSuggestions([{ ...suggestion, source_revision: 9, node_revision: 10 }], refreshed),
    )
    .mockResolvedValueOnce(
      response({
        ...suggestion,
        value: { caption: "", tags: [], visible_text: "" },
      }),
    )
    .mockResolvedValueOnce(preparedSuggestions([], refreshed));
  vi.stubGlobal("fetch", fetch);
  ui(editor);
  await fireEvent.click(screen.getByRole("checkbox", { name: "A harbor." }));
  await fireEvent.click(screen.getByRole("button", { name: "Accept selected" }));
  await screen.findByRole("button", { name: "Reload metadata" });
  expect(editor.conflict).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Reload metadata" }));
  await vi.waitFor(() => expect(editor.conflict).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Accept selected" }));
  await vi.waitFor(() => expect(editor.suggestions).toEqual([]));
  expect(JSON.parse(fetch.mock.calls[2]![1].body)).toMatchObject({
    asset_id: "asset",
    asset_revision: 1,
    source_revision: 9,
    node_revision: 10,
  });
  editor.dispose();
});

it("conceals suggestion text when refresh removes a hidden target", async () => {
  const editor = editorWithTargets();
  editor.suggestions = [
    photoSuggestion({
      value: { caption: "A harbor.", tags: ["coast"], visible_text: "PRIVATE SIGN" },
    }),
  ];
  ui(editor);
  expect(screen.getByText("PRIVATE SIGN")).toBeTruthy();
  editor.conceal();
  editor.revalidate(new Set());
  await vi.waitFor(() => expect(screen.queryByText("PRIVATE SIGN")).toBeNull());
  expect(editor.suggestions).toEqual([]);
  editor.dispose();
});
function photoSuggestion(overrides: Partial<api.PhotoSuggestion>): api.PhotoSuggestion {
  return {
    suggestion_id: "suggestion",
    content_version_id: "version",
    target: "jpeg",
    asset_id: "asset",
    asset_revision: 1,
    source_revision: 1,
    node_revision: 1,
    value: { caption: "", tags: [], visible_text: "" },
    ...overrides,
  };
}

function preparedSuggestions(suggestions: api.PhotoSuggestion[], prepared = targets) {
  return response({ assets: [asset], targets: prepared, suggestions });
}

it("re-reads assigned tags after a failed edit keeps its draft", async () => {
  const access = new HiddenAccess("session", vi.fn());
  const editor = new PhotoAuthoredEditor("session", vi.fn(), undefined, access);
  editor.targets = structuredClone(targets).map((target) => ({ ...target, tags: ["assigned"] }));
  editor.asset = structuredClone(asset);
  const fetcher = vi.fn(async (url: string, init: RequestInit) => {
    init.signal?.throwIfAborted();
    return url.endsWith("/tags/assigned")
      ? response({ id: "assigned", name: "Assigned", revision: 1, assignment_count: 1 })
      : response({ detail: "Edit unavailable" }, 503);
  });
  vi.stubGlobal("fetch", fetcher);
  await editor.edit({ asset_ids: ["asset"] }, { rating: 5 }, false, vi.fn());
  expect(editor.dirty).toBe(true);
  await editor.refreshAssignedTags();
  expect(fetcher.mock.calls.filter(([url]) => String(url).endsWith("/tags/assigned"))).toHaveLength(
    1,
  );
  editor.dispose();
});

it.each(["a quiet prepare", "an assigned-tag refresh"] as const)(
  "drops a pending quiet metadata read when an edit starts: stalled keyword lookup of %s",
  async (read) => {
    const access = new HiddenAccess("session", vi.fn());
    const editor = new PhotoAuthoredEditor("session", vi.fn(), undefined, access);
    const tagged = structuredClone(targets).map((target) => ({ ...target, tags: ["unlisted"] }));
    const fetcher = vi.fn((url: string, init: RequestInit) => {
      if (url.endsWith("/prepare"))
        return Promise.resolve(response({ assets: [asset], targets: tagged }));
      if (url.endsWith("/tags/unlisted"))
        return new Promise<Response>((_, reject) =>
          init.signal?.addEventListener("abort", () => reject(init.signal!.reason), { once: true }),
        );
      return Promise.resolve(response({ receipt_id: "receipt", before: [], after: [] }));
    });
    vi.stubGlobal("fetch", fetcher);
    let reading: Promise<unknown>;
    if (read === "a quiet prepare")
      reading = editor.prepare({ asset_ids: ["asset"] }, false, false, true);
    else {
      editor.targets = tagged;
      reading = expect(editor.refreshAssignedTags()).rejects.toMatchObject({ name: "AbortError" });
    }
    await vi.waitFor(() =>
      expect(fetcher.mock.calls.some(([url]) => String(url).endsWith("/tags/unlisted"))).toBe(true),
    );
    void editor.edit({ asset_ids: ["asset"] }, { rating: 5 }, false, vi.fn());
    await vi.waitFor(
      () => expect(fetcher.mock.calls.some(([url]) => String(url).endsWith("/edit"))).toBe(true),
      { timeout: 150 },
    );
    await reading;
    editor.dispose();
  },
);
