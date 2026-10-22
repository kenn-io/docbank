import { photoFace, photoPersonAssertion, photoFacePage } from "./photo-test-fixtures.js";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { PhotoFaces } from "./photoFaces.svelte.js";
import PhotoPeople from "./PhotoPeople.svelte";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { person as personSummary } from "./people-test-fixtures.js";
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
beforeEach(() => {
  vi.spyOn(api, "listPersonFacePreviews").mockResolvedValue({ items: [] });
  vi.stubGlobal(
    "Image",
    class {
      naturalWidth = 1000;
      naturalHeight = 800;
      src = "";
      decode() {
        return Promise.resolve();
      }
    },
  );
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
});
const face = photoFace({ revision: 3, assertion_id: undefined });
it("marks existing people and creates a new person in the whole photo", async () => {
  const read = vi
    .spyOn(api, "listPhotoFaces")
    .mockResolvedValue({ faces: [], assertions: [], people: [] });
  const people = ["a", "b"].map((id) => ({
    person_id: `person-${id}`,
    display_name: "Alex",
    display_name_folded: "alex",
    state: "curated",
    revision: 1,
    created_at: "2026-01-01T00:00:00Z",
  }));
  const search = vi.spyOn(api, "listPeople").mockResolvedValue({ items: people });
  const write = vi.spyOn(api, "createPhotoFace").mockImplementation(async () => {
    const assertion = photoPersonAssertion({ person_id: "person-b", photo_mark: true });
    read.mockResolvedValue({
      faces: [],
      assertions: [assertion],
      people: [{ person_id: "person-b", display_name: "Alex", state: "curated" }],
    });
    return { assertion };
  });
  const changed = vi.fn();
  const faces = new PhotoFaces("session", vi.fn(), false, changed);
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Mark person in photo" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Alex · 2 people" }));
  const dialog = within(await screen.findByRole("dialog", { name: "Choose Alex" }));
  expect(dialog.queryByRole("radio", { name: /New person/ })).toBeNull();
  await fireEvent.click(dialog.getByRole("radio", { name: /person-b/ }));
  await fireEvent.click(dialog.getByRole("button", { name: "Mark person in photo" }));
  await waitFor(() =>
    expect(write).toHaveBeenCalledExactlyOnceWith(
      "version-a",
      { person_id: "person-b" },
      expect.objectContaining({ session: "session" }),
    ),
  );
  await screen.findByRole("button", { name: "Clear whole-photo mark" });
  expect(changed).toHaveBeenCalledOnce();
  await waitFor(() => expect(faces.busy).toBe(false));
  search.mockResolvedValue({ items: [people[1]!] });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Mark person in photo" })).toHaveProperty(
      "disabled",
      false,
    ),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Mark person in photo" }));
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Alex" }));
  await waitFor(() => expect(write).toHaveBeenCalledTimes(2));
  expect(write).toHaveBeenLastCalledWith("version-a", { person_id: "person-b" }, expect.anything());
  await waitFor(() => expect(faces.busy).toBe(false));
  await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());
  search.mockResolvedValue({ items: [] });
  await fireEvent.click(screen.getByRole("button", { name: "Mark person in photo" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "New name" } });
  await waitFor(() =>
    expect(search).toHaveBeenLastCalledWith(
      { query: "New name", limit: 100, recent_first: true },
      expect.objectContaining({ session: "session" }),
    ),
  );
  await fireEvent.mouseDown(
    await screen.findByRole("option", { name: 'Create person "New name"' }),
  );
  await waitFor(() =>
    expect(write).toHaveBeenLastCalledWith(
      "version-a",
      expect.objectContaining({ person_name: "New name", person_id: expect.any(String) }),
      expect.objectContaining({ session: "session" }),
    ),
  );
  faces.dispose();
});
it("names the face selected by its numbered picker title", async () => {
  const second = photoFace({
    face_id: "face-second",
    assertion_id: undefined,
    revision: 5,
    created_at: "2026-02-01T00:00:00Z",
  });
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue(
    photoFacePage({ faces: [face, second], assertions: [], people: [] }),
  );
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [] });
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  expect(screen.getByTitle("Name face 1")).toBeTruthy();
  await fireEvent.click(screen.getByTitle("Name face 2"));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Grace" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create person "Grace"' }));
  await waitFor(() =>
    expect(write).toHaveBeenCalledExactlyOnceWith(
      "version-a",
      expect.objectContaining({ face_id: "face-second", revision: 5, person_name: "Grace" }),
      expect.anything(),
    ),
  );
  faces.dispose();
});

it("resets validated person search input on reopen", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [face], assertions: [], people: [] });
  const search = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [] });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "雪".repeat(67) } });
  await screen.findByText("Name must be 200 bytes or fewer.");
  expect(search.mock.calls.some(([params]) => params?.query === "雪".repeat(67))).toBe(false);
  const normalized = "雪".repeat(66);
  search.mockResolvedValue({ items: [] });
  await fireEvent.input(screen.getByRole("combobox"), {
    target: { value: normalized + "   " },
  });
  expect(await screen.findByRole("option", { name: `Create person "${normalized}"` })).toBeTruthy();
  expect(search).toHaveBeenLastCalledWith(
    expect.objectContaining({ query: normalized, limit: 100 }),
    expect.objectContaining({ session: "session" }),
  );
  await fireEvent.keyDown(screen.getByRole("combobox"), { key: "Escape" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await waitFor(() =>
    expect(search).toHaveBeenLastCalledWith(
      expect.objectContaining({ query: "", limit: 100 }),
      expect.objectContaining({ session: "session" }),
    ),
  );
  faces.dispose();
});
it("searches again after a failed assignment and disables the picker throughout creation", async () => {
  const read = vi
    .spyOn(api, "listPhotoFaces")
    .mockResolvedValue({ faces: [face], assertions: [], people: [] });
  let canonical = "";
  const search = vi.spyOn(api, "listPeople").mockImplementation(async ({ query } = {}) => ({
    items:
      canonical && canonical.toLowerCase() === query?.trim().toLowerCase()
        ? [
            {
              person_id: "person-new",
              display_name: canonical,
              state: "curated",
              display_name_folded: canonical.toLowerCase(),
              created_at: "2026-01-01T00:00:00Z",
              revision: 1,
            },
          ]
        : [],
  }));
  let fail!: (error: Error) => void;
  const write = vi
    .spyOn(api, "decidePhotoFace")
    .mockImplementationOnce(
      () =>
        new Promise((_resolve, reject) => {
          fail = reject;
        }),
    )
    .mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Ada" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create person "Ada"' }));
  await waitFor(() => expect(write).toHaveBeenCalledOnce());
  expect((screen.getByRole("combobox") as HTMLInputElement).disabled).toBe(true);
  canonical = "Ada";
  read.mockResolvedValue({
    faces: [{ ...face, revision: 4, assertion_id: "assertion" }],
    assertions: [photoPersonAssertion({ assertion_id: "assertion", person_id: "person-new" })],
    people: [{ person_id: "person-new", display_name: "Ada", state: "curated" }],
  });
  search.mockClear();
  fail(new Error("Response lost"));
  await screen.findByText("Response lost");
  await waitFor(() =>
    expect(search).toHaveBeenCalledWith(
      expect.objectContaining({ query: "Ada", limit: 100 }),
      expect.objectContaining({ session: "session" }),
    ),
  );

  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "ada" } });
  await screen.findByRole("option", { name: 'Create person "ada"' });
  await fireEvent.mouseDown(screen.getByRole("option", { name: "Ada" }));
  await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());
  expect(write).toHaveBeenCalledTimes(1);
  faces.dispose();
});
it("replaces a cached selected name after canonical names reload", async () => {
  const page = photoFacePage();
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue(page);
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [] });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session", onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Ada" }));
  await fireEvent.keyDown(screen.getByRole("combobox"), { key: "Escape" });
  vi.mocked(api.listPhotoFaces).mockResolvedValue({
    ...page,
    people: [{ ...page.people[0]!, display_name: "Ada Updated" }],
  });
  await faces.load("version-a");
  await screen.findByRole("button", { name: "Find or create a person: Ada Updated" });
  expect(screen.getByRole("button", { name: "Ada Updated" })).toBeTruthy();
  faces.dispose();
});

it("keeps retired labels and correction controls without Library links", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue(
    photoFacePage({
      assertions: [photoPersonAssertion({ photo_mark: true })],
      people: [{ person_id: "person-a", display_name: "Ada", state: "retired" }],
    }),
  );
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session", onperson: vi.fn() });
  expect(screen.getAllByText("Ada", { selector: "span" })).toHaveLength(2);
  expect(screen.queryByRole("button", { name: "Ada" })).toBeNull();
  expect(screen.getByRole("button", { name: "Remove face" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Clear whole-photo mark" })).toBeTruthy();
  faces.dispose();
});

it.each(["distinct", "tied-whole", "tied-empty", "empty", "image500", "switch"])(
  "uses one namesake projection with crop and whole-photo rows: %s",
  async (mode) => {
    const tied = mode.startsWith("tied");
    vi.spyOn(api, "listPhotoFaces").mockResolvedValue(
      photoFacePage({ faces: [face], assertions: [], people: [] }),
    );
    vi.spyOn(api, "listPeople").mockResolvedValue({
      items: ["a", "b"].map((id) => ({
        person_id: `person-${id}`,
        display_name: "Alex",
        display_name_folded: "alex",
        state: "curated",
        revision: 1,
        created_at: "2026-01-01T00:00:00Z",
        identity_label: tied ? "" : `${id}@example.test`,
      })),
    });
    const list = vi.mocked(api.listPersonFacePreviews).mockImplementation(async (id) => ({
      items:
        mode === "switch"
          ? Array.from({ length: 3 }, (_, index) => ({
              asset_id: `${id}-photo-${index}`,
              content_version_id: `${id}-raw-${index}`,
              generation_id: "",
              rotation: 0,
              box: { x: 0.1, y: 0.2, width: 0.1, height: 0.1 },
            }))
          : mode === "empty" || (mode === "tied-empty" && id === "person-b")
            ? []
            : [
                {
                  asset_id: "photo-current",
                  content_version_id: "version-a",
                  generation_id: "grid-current",
                  rotation: 90,
                  ...(id === "person-a"
                    ? { box: { x: 0.1, y: 0.2, width: 0.1, height: 0.1 } }
                    : {}),
                },
              ],
    }));
    const { PhotoPreviewCache } = await import("./photoPreviewCache.js");
    const cache = new PhotoPreviewCache("session", vi.fn());
    const read = vi.spyOn(cache, "get").mockResolvedValue(new Blob(["synthetic"]));
    if (mode === "image500") {
      const { APIError } = await import("./api-transport.js");
      read.mockRejectedValue(new APIError("Preview failed", 500, "preview_failed"));
    }
    const prepare =
      mode === "switch"
        ? vi
            .spyOn(api, "preparePhotoPreview")
            .mockResolvedValue({ state: "ready", generation_id: "grid-source" })
        : undefined;
    let finishImage!: () => void;
    let decoded = 0;
    if (mode === "switch")
      vi.stubGlobal(
        "Image",
        class {
          naturalWidth = 1000;
          naturalHeight = 800;
          src = "";
          decode() {
            decoded++;
            return decoded === 2
              ? new Promise<void>((resolve) => {
                  finishImage = resolve;
                })
              : Promise.resolve();
          }
        },
      );
    vi.stubGlobal(
      "URL",
      class extends URL {
        static createObjectURL = vi.fn(() => "blob:alex");
        static revokeObjectURL = vi.fn();
      },
    );
    const faces = new PhotoFaces("session", vi.fn());
    await faces.load("version-a");
    render(PhotoPeople, { faces, session: "session", cache });
    await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
    await fireEvent.mouseDown(await screen.findByRole("option", { name: "Alex · 2 people" }));
    expect(list).not.toHaveBeenCalled();
    if (mode === "switch") {
      const dialog = within(await screen.findByRole("dialog", { name: "Choose Alex" }));
      expect(prepare).not.toHaveBeenCalled();
      await fireEvent.click(dialog.getAllByRole("button", { name: "Photos" })[0]!);
      await waitFor(() => expect(finishImage).toBeTypeOf("function"));
      expect(dialog.getAllByRole("img")).toHaveLength(1);
      const stale = list.mock.calls[0]![2]!.signal!;
      await fireEvent.click(dialog.getAllByRole("button", { name: "Photos" })[1]!);
      expect(stale.aborted).toBe(true);
      expect(URL.revokeObjectURL).toHaveBeenCalledTimes(2);
      expect(read.mock.calls.slice(0, 2).every((call) => call[2]!.aborted)).toBe(true);
      await act(() => finishImage());
      await waitFor(() => expect(dialog.getAllByRole("img")).toHaveLength(3));
      expect(list.mock.calls.map(([id]) => id)).toEqual(["person-a", "person-b"]);
      expect(
        prepare!.mock.calls.filter(([, params]) => params.person_id === "person-b"),
      ).toHaveLength(3);
      expect(prepare).toHaveBeenLastCalledWith(
        "person-b-photo-2",
        {
          content_version_id: "person-b-raw-2",
          person_id: "person-b",
          size: "grid",
          hidden: false,
        },
        expect.anything(),
      );
    } else {
      for (const [index, id] of ["person-a", "person-b"].entries()) {
        await fireEvent.click(screen.getAllByRole("button", { name: "Photos" })[index]!);
        await waitFor(() => expect(screen.queryByText("Loading photos…")).toBeNull());
        expect(list).toHaveBeenCalledWith(
          id,
          { hidden: false },
          expect.objectContaining({ session: "session" }),
        );
        if (mode === "empty" || (mode === "tied-empty" && index === 1)) {
          expect(
            screen.getByText(tied ? "Preview unavailable" : "No photos you can see."),
          ).toBeTruthy();
        } else if (mode === "image500" || (tied && index === 1)) {
          expect(screen.getByText("Preview unavailable")).toBeTruthy();
        } else {
          expect(screen.getAllByRole("img")).toHaveLength(1);
          if (index === 0) {
            expect(screen.getByRole("img").style.transform).toContain("rotate(90deg)");
            expect(read).toHaveBeenCalledWith(
              "photo-current",
              "grid-current",
              expect.anything(),
              false,
              id,
            );
          }
        }
      }
      expect(list).toHaveBeenCalledTimes(2);
      if (mode === "empty") expect(read).not.toHaveBeenCalled();
    }
    await cache.dispose();
    faces.dispose();
  },
);

it.each(["select", "cancel", "scope", "unmount", "projection403", "image403"])(
  "bounds namesake preview reads within Hidden access: %s",
  async (mode) => {
    const { HiddenAccess } = await import("./hiddenAccess.svelte.js");
    const lost = vi.fn();
    const access = new HiddenAccess("session", lost);
    access.acceptState({ configured: true, expires_in: 60 }, Date.now());
    vi.spyOn(api, "listPhotoFaces").mockResolvedValue({
      faces: [face],
      assertions: [],
      people: [],
    });
    vi.spyOn(api, "listPeople").mockResolvedValue({
      items: Array.from({ length: 6 }, (_, index) => ({
        person_id: `person-${index}`,
        display_name: "Alex",
        display_name_folded: "alex",
        state: "curated",
        revision: 1,
        created_at: "2026-01-01T00:00:00Z",
        identity_label: `alex-${index}@example.test`,
      })),
    });
    let active = 0;
    let maximum = 0;
    const pending = new Map<
      string,
      { finish: (fail?: boolean | Error, preview?: boolean) => void; signal: AbortSignal }
    >();
    const pendingRead = (id: string, options?: { signal?: AbortSignal | null }) => {
      active++;
      maximum = Math.max(maximum, active);
      return new Promise<api.PersonFacePreviewPage>((resolve, reject) => {
        pending.set(id, {
          signal: options!.signal!,
          finish(fail, preview) {
            active--;
            pending.delete(id);
            if (fail) reject(fail instanceof Error ? fail : new Error("Preview read failed"));
            else
              resolve({
                items: preview
                  ? [
                      {
                        asset_id: "photo-a",
                        content_version_id: "version-a",
                        generation_id: "grid-a",
                        rotation: 0,
                        box: { x: 0.1, y: 0.2, width: 0.1, height: 0.1 },
                      },
                    ]
                  : [],
              });
          },
        });
      });
    };
    const list = vi.mocked(api.listPersonFacePreviews).mockImplementation((id, params, options) => {
      expect(params?.hidden).toBe(true);
      return pendingRead(id, options);
    });
    const lockRequest = vi.spyOn(api, "lockPhotoHidden").mockResolvedValue({ configured: true });
    const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
    const faces = new PhotoFaces("session", lost, true, vi.fn(), access);
    await faces.load("version-a");
    const { PhotoPreviewCache } = await import("./photoPreviewCache.js");
    const cache = new PhotoPreviewCache("session", lost, true);
    const read = vi.spyOn(cache, "get").mockResolvedValue(new Blob(["synthetic"]));
    if (mode === "image403") {
      const { APIError } = await import("./api-transport.js");
      read.mockRejectedValue(new APIError("Preview failed", 403, "preview_failed"));
    }
    vi.stubGlobal(
      "URL",
      class extends URL {
        static createObjectURL = vi.fn(() => "blob:alex-cancel");
        static revokeObjectURL = vi.fn();
      },
    );
    const component = render(PhotoPeople, { faces, session: "session", cache });
    await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
    const group = await screen.findByRole("option", { name: "Alex · 6 people" });
    await fireEvent.mouseDown(group);
    expect(list).not.toHaveBeenCalled();
    await fireEvent.click(screen.getAllByRole("button", { name: "Photos" })[0]!);
    await waitFor(() => expect(list).toHaveBeenCalledTimes(1));
    expect(maximum).toBe(1);
    if (mode.endsWith("403")) {
      const { APIError } = await import("./api-transport.js");
      const reads = [...pending.values()];
      await act(() =>
        pending
          .get("person-0")!
          .finish(
            mode === "projection403"
              ? new APIError("Hidden locked", 403, "photo_hidden_locked")
              : false,
            mode === "image403",
          ),
      );
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(lost).toHaveBeenCalledWith(expect.objectContaining({ status: 403 }));
      expect(reads.every((read) => read.signal.aborted)).toBe(true);
      await act(async () => {
        for (const read of pending.values()) {
          expect(read.signal.aborted).toBe(true);
          read.finish();
        }
        await access.settled();
      });
      expect(maximum).toBe(1);
      expect(write).not.toHaveBeenCalled();
    } else if (mode === "select") {
      await act(() => pending.get("person-0")!.finish(true));
      await screen.findByText("Preview unavailable");
      await fireEvent.click(screen.getAllByRole("button", { name: "Photos" })[1]!);
      await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
      await act(async () => {
        pending.get("person-1")!.finish();
        await access.settled();
      });
      expect(maximum).toBe(1);
    } else {
      if (mode === "cancel" || mode === "unmount") {
        await act(() => pending.get("person-0")!.finish(false, true));
        expect((await screen.findByRole("img")).getAttribute("src")).toBe("blob:alex-cancel");
        expect(URL.revokeObjectURL).not.toHaveBeenCalled();
        await fireEvent.click(screen.getAllByRole("button", { name: "Photos" })[1]!);
        await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
      }
      const lock = mode === "scope" ? access.lock() : undefined;
      const reads = [...pending.values()];
      expect(reads.length).toBeGreaterThan(0);
      if (mode === "cancel") await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
      else if (mode === "unmount") component.unmount();
      else await act(() => access.acceptState({ configured: true }, Date.now() + 1));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(reads.every((read) => read.signal.aborted)).toBe(true);
      if (mode === "scope") expect(lockRequest).not.toHaveBeenCalled();
      await act(async () => {
        for (const read of reads) read.finish();
        await lock;
      });
      if (mode === "scope") expect(lockRequest).toHaveBeenCalledOnce();
      else expect(lockRequest).not.toHaveBeenCalled();
      expect(list).toHaveBeenCalledTimes(mode === "scope" ? 1 : 2);
      if (mode !== "scope") expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:alex-cancel");
      expect(write).not.toHaveBeenCalled();
    }
    await cache.dispose();
    access.dispose();
    faces.dispose();
  },
);

it.each([
  {
    label: "distinct identities",
    identity: "alex-a@example.test",
    secondIdentity: "alex-b@example.test",
    secondTime: "12:00:00",
  },
  { label: "same displayed date", identity: "", secondTime: "12:00:30" },
  { label: "new person", identity: "", secondTime: "12:00:30", create: true },
])(
  "distinguishes namesakes for $label",
  async ({ identity, secondIdentity, secondTime, create }) => {
    const ids = ["person-alex-a", "person-alex-b", "person-alex-c"];
    const identities = [
      identity,
      secondIdentity ?? identity,
      secondIdentity ? "" : "other@example.test",
    ];
    vi.spyOn(api, "listPhotoFaces").mockResolvedValue({
      faces: [face],
      assertions: [],
      people: [],
    });
    vi.spyOn(api, "listPeople").mockResolvedValue({
      next_cursor: "different-name",
      items: ids.map((person_id, index) => ({
        person_id,
        display_name: "Alex",
        display_name_folded: "alex",
        created_at: `2026-01-01T${index === 1 ? secondTime : "12:00:00"}Z`,
        identity_label: identities[index],
        revision: 1,
        state: "curated",
      })),
    });

    const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
    const faces = new PhotoFaces("session", vi.fn());
    await faces.load("version-a");
    render(PhotoPeople, { faces, session: "session" });
    await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
    if (create) await fireEvent.input(screen.getByRole("combobox"), { target: { value: "alex" } });
    await fireEvent.mouseDown(await screen.findByRole("option", { name: "Alex · 3 people" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Choose Alex" }));
    if (secondIdentity) {
      expect(dialog.getByText("alex-a@example.test")).toBeTruthy();
      expect(dialog.getByText(new Date("2026-01-01T12:00:00Z").toLocaleDateString())).toBeTruthy();
      expect(dialog.getByText("person-alex-a")).toBeTruthy();
    } else {
      expect(dialog.getByText("person-alex-a")).toBeTruthy();
      expect(dialog.getByText("other@example.test")).toBeTruthy();
    }
    expect(dialog.getByText("person-alex-c")).toBeTruthy();
    await fireEvent.click(
      dialog.getByRole("radio", {
        name: create
          ? "New person named alex"
          : secondIdentity
            ? /alex-b@example.test/
            : /person-alex-b/,
      }),
    );
    await fireEvent.click(dialog.getByRole("button", { name: "Name face" }));
    await waitFor(() =>
      expect(write).toHaveBeenCalledWith(
        "version-a",
        expect.objectContaining(
          create
            ? { face_id: face.face_id, revision: 3, person_name: "alex" }
            : { person_id: "person-alex-b" },
        ),
        expect.anything(),
      ),
    );
    faces.dispose();
  },
);

it("shows the API's photo capacity refusal after choosing a person", async () => {
  const read = vi.spyOn(api, "listPhotoFaces").mockResolvedValue(photoFacePage());
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [] });
  const message = "photo version already has 1,000 faces or 1,000 depicted people";
  const write = vi
    .spyOn(api, "createPhotoFace")
    .mockRejectedValue(new APIError(message, 409, "photo_face_limit"));
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Mark person in photo" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Grace" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create person "Grace"' }));
  expect(await screen.findByText(message)).toBeTruthy();
  expect(write).toHaveBeenCalledExactlyOnceWith(
    "version-a",
    expect.objectContaining({ person_name: "Grace", person_id: expect.any(String) }),
    expect.anything(),
  );
  expect(read).toHaveBeenCalledTimes(2);
  faces.dispose();
});

it.each(["face", "mark"])("confirms %s removal with the captured revision", async (target) => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue(
    photoFacePage({ assertions: [photoPersonAssertion({ photo_mark: true })] }),
  );
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  render(PhotoPeople, { faces, session: "session" });
  const label = target === "face" ? "Remove face" : "Clear whole-photo mark";
  await fireEvent.click(screen.getByRole("button", { name: label }));
  await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(write).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: label }));
  const dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain(
    target === "face" ? "You can't undo this here." : "Remove Ada from this photo?",
  );
  faces.faces = faces.faces.map((row) => ({ ...row, revision: 90 }));
  faces.assertions = faces.assertions.map((row) => ({ ...row, revision: 90 }));
  await fireEvent.click(within(dialog).getByRole("button", { name: label }));
  await waitFor(() =>
    expect(write).toHaveBeenCalledWith(
      "version-a",
      expect.objectContaining(
        target === "face"
          ? { action: "dismiss", face_id: "face-a", revision: 7 }
          : { action: "retract", assertion_id: "assertion-a", revision: 1 },
      ),
      expect.anything(),
    ),
  );
  faces.dispose();
});

it.each(["exact", "absent", "broad", "case"])(
  "finishes paginated person searches: %s",
  async (result) => {
    vi.spyOn(api, "listPhotoFaces").mockResolvedValue({
      faces: [face],
      assertions: [],
      people: [],
    });
    const grouped = result === "broad" || result === "case";
    const query = result === "broad" ? "Al" : "Alex";
    const person = (index: number): api.PersonSummary => ({
      person_id: `person-${index}`,
      display_name: "Alex",
      display_name_folded: "alex",
      state: "curated",
      revision: 1,
      created_at: "2026-01-01T00:00:00Z",
    });
    let finish!: (page: api.PersonPage) => void;
    let attempts = 0;
    const lost = vi.fn();
    const search = vi
      .spyOn(api, "listPeople")
      .mockImplementation(async ({ query, cursor } = {}) => {
        if (!query) return { items: [] };
        if (!cursor)
          return {
            items: Array.from({ length: 100 }, (_, index) => ({
              ...person(index),
              ...(grouped
                ? {}
                : {
                    display_name: `Aaron ${index} Alex`,
                    display_name_folded: `aaron ${index} alex`,
                  }),
            })),
            next_cursor: "tail",
            same_name_continues: grouped,
          };
        if (!grouped && ++attempts === 1) {
          const { APIError } = await import("./api-transport.js");
          throw new APIError("People search unavailable", 500, "search_rejected");
        }
        return new Promise((resolve) => {
          finish = resolve;
        });
      });
    const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
    const faces = new PhotoFaces("session", lost);
    await faces.load("version-a");
    render(PhotoPeople, { faces, session: "session" });
    await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: query } });
    const more = await screen.findByRole("option", { name: "Load more" });
    expect(search.mock.calls.filter(([params]) => params?.query === query)).toHaveLength(1);
    expect(screen.queryByRole("option", { name: `Create person "${query}"` })).toBeNull();
    await fireEvent.mouseDown(more);
    if (!grouped) {
      await screen.findByText("People search unavailable");
      expect(lost).not.toHaveBeenCalled();
      expect(screen.getByRole("option", { name: "Aaron 0 Alex" })).toBeTruthy();
      expect(screen.queryByRole("option", { name: 'Create person "Alex"' })).toBeNull();
      await fireEvent.mouseDown(screen.getByRole("option", { name: "Retry" }));
    }
    await waitFor(() => expect(finish).toBeTypeOf("function"));
    expect(screen.queryByRole("option", { name: `Create person "${query}"` })).toBeNull();
    if (grouped) expect(screen.queryByRole("option", { name: /Alex/ })).toBeNull();
    if (result === "case") {
      await act(() =>
        finish({
          items: Array.from({ length: 100 }, (_, i) => person(100 + i)),
          next_cursor: "last",
          same_name_continues: true,
        }),
      );
      await fireEvent.mouseDown(await screen.findByRole("option", { name: "Load more" }));
      await waitFor(() =>
        expect(search).toHaveBeenCalledWith(
          expect.objectContaining({ query, limit: 100, cursor: "last" }),
          expect.anything(),
        ),
      );
      expect(screen.queryByRole("option", { name: `Create person "${query}"` })).toBeNull();
    }
    await act(() =>
      finish({
        items:
          result === "absent"
            ? []
            : result === "exact"
              ? [person(100)]
              : [
                  ...(result === "case"
                    ? Array.from({ length: 100 }, (_, i) => ({
                        ...person(200 + i),
                        display_name: "ALEX",
                      }))
                    : [person(100)]),
                  ...(result === "broad"
                    ? [{ ...person(101), display_name: "Alice", display_name_folded: "alice" }]
                    : []),
                ],
        next_cursor: result === "broad" ? "more" : undefined,
      }),
    );
    if (grouped) {
      await fireEvent.mouseDown(
        await screen.findByRole("option", {
          name: `Alex · ${result === "case" ? 300 : 101} people`,
        }),
      );
      const dialog = await screen.findByRole("dialog", { name: "Choose Alex" });
      const selectedID = result === "case" ? "person-299" : "person-100";
      await fireEvent.click(dialog.querySelector(`input[value="${selectedID}"]`)!);
      await fireEvent.click(screen.getByRole("button", { name: "Name face" }));
      await waitFor(() =>
        expect(write).toHaveBeenCalledWith(
          "version-a",
          expect.objectContaining({ person_id: selectedID }),
          expect.anything(),
        ),
      );
    } else {
      await screen.findByRole("option", { name: 'Create person "Alex"' });
      expect(screen.queryByText("People search unavailable")).toBeNull();
      await fireEvent.mouseDown(
        await screen.findByRole("option", {
          name: result === "exact" ? "Alex" : 'Create person "Alex"',
        }),
      );
      await waitFor(() =>
        expect(write).toHaveBeenCalledWith(
          "version-a",
          expect.objectContaining(
            result === "exact" ? { person_id: "person-100" } : { person_name: "Alex" },
          ),
          expect.anything(),
        ),
      );
    }
    expect(search).toHaveBeenCalledWith(
      expect.objectContaining({ query, limit: 100, cursor: "tail" }),
      expect.anything(),
    );
    expect(search.mock.calls.filter(([params]) => params?.query === query)).toHaveLength(
      result === "broad" ? 2 : 3,
    );
    faces.dispose();
  },
);

it.each(["query", "Hidden scope"])(
  "ignores a stale people search response after a new %s",
  async (trigger) => {
    const { HiddenAccess } = await import("./hiddenAccess.svelte.js");
    const access = trigger === "Hidden scope" ? new HiddenAccess("session", vi.fn()) : undefined;
    access?.acceptState({ configured: true, expires_in: 60 }, Date.now());
    vi.spyOn(api, "listPhotoFaces").mockResolvedValue({
      faces: [face],
      assertions: [],
      people: [],
    });
    const person: api.PersonSummary = {
      person_id: "person-a",
      display_name: "Alex",
      display_name_folded: "alex",
      state: "curated",
      revision: 1,
      created_at: "2026-01-01T00:00:00Z",
    };
    let finish!: (page: api.PersonPage) => void;
    vi.spyOn(api, "listPeople").mockImplementation(async ({ query, cursor } = {}) => {
      if (access || cursor)
        return new Promise((resolve) => {
          finish = resolve;
        });
      return query === "Alex"
        ? { items: [person], next_cursor: "tail", same_name_continues: true }
        : { items: [{ ...person, display_name: "Bea", display_name_folded: "bea" }] };
    });
    const faces = new PhotoFaces("session", vi.fn(), false, undefined, access);
    await faces.load("version-a");
    render(PhotoPeople, { faces, session: "session" });
    await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
    if (!access)
      await fireEvent.mouseDown(await screen.findByRole("option", { name: "Load more" }));
    await waitFor(() => expect(finish).toBeTypeOf("function"));
    if (access) await act(() => access.acceptState({ configured: true }, Date.now() + 1));
    else {
      await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Bea" } });
      await screen.findByRole("option", { name: "Bea" });
    }
    await act(() => finish({ items: [person] }));
    await waitFor(() => expect(screen.queryByRole("option", { name: /Alex/ })).toBeNull());
    if (access) expect(screen.getByRole("combobox")).toBeTruthy();
    else expect(screen.getByRole("option", { name: "Bea" })).toBeTruthy();
    faces.dispose();
    access?.dispose();
  },
);

it.each(["unauthorized", "generic"])(
  "handles a %s first-page people search failure",
  async (failure) => {
    const { APIError } = await import("./api-transport.js");
    const cause =
      failure === "unauthorized"
        ? new APIError("Search rejected", 401, "search_rejected")
        : new Error("People search unavailable");
    const name = failure === "unauthorized" ? "Alex" : "Ada";
    vi.spyOn(api, "listPhotoFaces").mockResolvedValue({
      faces: [face],
      assertions: [],
      people: [],
    });
    const search = vi.spyOn(api, "listPeople").mockImplementation(async ({ query } = {}) => {
      if (query === name) throw cause;
      return { items: [] };
    });
    vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
    const lost = vi.fn();
    const faces = new PhotoFaces("session", lost);
    await faces.load("version-a");
    render(PhotoPeople, { faces, session: "session" });
    await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: name } });
    await screen.findByText(cause.message);
    if (failure === "unauthorized") {
      expect(lost).toHaveBeenCalledExactlyOnceWith(cause);
      expect(screen.queryByRole("option", { name: /Alex/ })).toBeNull();
    } else {
      expect(screen.queryByRole("option", { name: 'Create person "Ada"' })).toBeNull();
      search.mockResolvedValue({ items: [] });
      await fireEvent.mouseDown(await screen.findByRole("option", { name: "Retry" }));
      expect(search).toHaveBeenLastCalledWith(
        expect.objectContaining({ query: "Ada", limit: 100 }),
        expect.objectContaining({ session: "session" }),
      );
      await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create person "Ada"' }));
      expect(screen.getByText(/A new person may be a duplicate/)).toBeTruthy();
    }
    faces.dispose();
  },
);
it("lists a person once when a rank change repeats them on the next page", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [], assertions: [], people: [] });
  vi.spyOn(api, "listPeople")
    .mockResolvedValueOnce({
      items: [personSummary("ada", { display_name: "Ada" })],
      next_cursor: "next",
    })
    .mockResolvedValueOnce({
      items: [
        personSummary("ada", { display_name: "Ada" }),
        personSummary("bob", { display_name: "Bob" }),
      ],
    });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Mark person in photo" }));
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Load more" }));
  await screen.findByRole("option", { name: "Bob" });
  expect(screen.getByRole("option", { name: "Ada" })).toBeTruthy();
  expect(screen.queryByRole("option", { name: "Ada · 2 people" })).toBeNull();
});
