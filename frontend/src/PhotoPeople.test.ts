import { photo, photoFace, photoPersonAssertion, photoFacePage } from "./photo-test-fixtures.js";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { PhotoFaces } from "./photoFaces.svelte.js";
import PhotoPeople from "./PhotoPeople.svelte";
import * as api from "./generated/docbank.js";
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
beforeEach(() =>
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  ),
);
const face = photoFace({ revision: 3, assertion_id: undefined });
it("validates and trims names, resets search on reopen, and opens a selected person’s photos", async () => {
  const assertion = photoPersonAssertion();
  vi.spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce({ faces: [face], assertions: [], people: [] })
    .mockResolvedValue({
      faces: [{ ...face, assertion_id: "assertion-a", revision: 1 }],
      assertions: [assertion],
      people: [{ person_id: "person-a", display_name: "Ada", state: "curated" }],
    });
  const search = vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [
      {
        person_id: "person-a",
        display_name: "Ada",
        revision: 1,
        display_name_folded: "ada",
        created_at: "2026-01-01T00:00:00Z",
        state: "curated",
      },
    ],
  });
  vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt-a" });
  const onperson = vi.fn(),
    faces = new PhotoFaces("session", vi.fn());
  const name = vi.spyOn(faces, "name");
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session", onperson });
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
    { query: normalized, limit: 100 },
    expect.objectContaining({ session: "session" }),
  );
  search.mockResolvedValue({
    items: [
      {
        person_id: "person-a",
        display_name: "Ada",
        revision: 1,
        display_name_folded: "ada",
        created_at: "2026-01-01T00:00:00Z",
        state: "curated",
      },
    ],
  });
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Ad" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Ada" }));
  await waitFor(() => expect(name).toHaveBeenCalledWith(face, "person-a"));
  await fireEvent.click(await screen.findByRole("button", { name: "Ada" }));
  expect(onperson).toHaveBeenCalledWith("person-a", "Ada");
  expect(search).toHaveBeenCalledWith(
    { query: "Ad", limit: 100 },
    expect.objectContaining({ session: "session" }),
  );
  search.mockResolvedValue({ items: [] });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Ada" }));
  await waitFor(() =>
    expect(search).toHaveBeenLastCalledWith(
      { query: "", limit: 100 },
      expect.objectContaining({ session: "session" }),
    ),
  );
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Ada" }));
  await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());
  expect(name).toHaveBeenCalledTimes(1);
  faces.dispose();
});
it("searches again after a failed assignment and disables the picker throughout creation", async () => {
  const read = vi
    .spyOn(api, "listPhotoFaces")
    .mockResolvedValue({ faces: [face], assertions: [], people: [] });
  let canonical = "";
  vi.spyOn(api, "listPeople").mockImplementation(async ({ query } = {}) => ({
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
  fail(new Error("Response lost"));
  await screen.findByText("Response lost");
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "ada" } });
  await screen.findByRole("option", { name: 'Create person "ada"' });
  await fireEvent.mouseDown(screen.getByRole("option", { name: "Ada" }));
  await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());
  expect(write).toHaveBeenCalledTimes(1);
  faces.dispose();
});
it("replaces a cached selected name when another face's decision reloads canonical names", async () => {
  const first = photoFace({ assertion_id: undefined });
  const second = photoFace({ face_id: "face-b", assertion_id: undefined });
  vi.spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce(photoFacePage({ faces: [first, second], assertions: [], people: [] }))
    .mockResolvedValueOnce(photoFacePage({ faces: [photoFace(), second] }))
    .mockResolvedValue(
      photoFacePage({
        faces: [photoFace(), { ...second, assertion_id: "assertion-b" }],
        assertions: [
          photoPersonAssertion(),
          photoPersonAssertion({ assertion_id: "assertion-b", person_id: "person-b" }),
        ],
        people: [
          { person_id: "person-a", display_name: "Ada Updated", state: "curated" },
          { person_id: "person-b", display_name: "Grace", state: "curated" },
        ],
      }),
    );
  vi.spyOn(api, "listPeople").mockImplementation(async ({ query } = {}) => ({
    items: [
      {
        person_id: query === "Ada" ? "person-a" : "person-b",
        display_name: query!,
        state: "curated",
        revision: 1,
        display_name_folded: query!.toLowerCase(),
        created_at: "2026-01-01T00:00:00Z",
      },
    ],
  }));
  vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session", onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Ada" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Ada" }));
  await screen.findByRole("button", { name: "Find or create a person: Ada" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 2" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Grace" } });
  await screen.findByRole("option", { name: "Grace" });
  expect(screen.getByRole("button", { name: "Find or create a person: Ada" })).toBeTruthy();
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Grace" }));
  await screen.findByRole("button", { name: "Find or create a person: Ada Updated" });
  expect(screen.getByRole("button", { name: "Ada Updated" })).toBeTruthy();
  await screen.findByRole("button", { name: "Find or create a person: Grace" });
  faces.dispose();
});

it("retries an empty failed face page", async () => {
  vi.spyOn(api, "listPhotoFaces")
    .mockRejectedValueOnce(new Error("Offline"))
    .mockResolvedValue(photoFacePage());
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("button", { name: "Find or create a person: Ada" });
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
  expect(screen.queryByRole("button", { name: "Ada" })).toBeNull();
  expect(screen.getByRole("button", { name: "Remove face" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Clear whole-photo mark" })).toBeTruthy();
  faces.dispose();
});

it.each(["person-b", "new", "cancel", "assigned"])(
  "chooses a same-name person using linked photos: %s",
  async (selection) => {
    const { PhotoPreviewCache } = await import("./photoPreviewCache.js");
    const assigned = selection === "assigned";
    vi.spyOn(api, "listPhotoFaces").mockResolvedValue({
      faces: [assigned ? { ...face, assertion_id: "assertion-a" } : face],
      assertions: assigned ? [photoPersonAssertion()] : [],
      people: assigned ? [{ person_id: "person-a", display_name: "Alex", state: "curated" }] : [],
    });
    vi.spyOn(api, "listPeople").mockResolvedValue({
      items: [
        {
          person_id: "person-a",
          display_name: "Alex",
          state: "curated",
          revision: 1,
          display_name_folded: "alex",
          created_at: "2025-01-01T00:00:00Z",
        },
        {
          person_id: "person-b",
          display_name: "Alex",
          state: "curated",
          revision: 1,
          display_name_folded: "alex",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    const list = vi.spyOn(api, "listPhotoAssets").mockImplementation(async (request) => {
      const id = request.query.filters!.person_ids![0] === "person-a" ? 1 : 2;
      const row = photo(id);
      row.previews.grid = { state: "ready", generation_id: `grid-${id}` };
      return {
        items: [
          { ...row, asset_id: "photo-current", content_version_id: "version-current-raw" },
          row,
        ],
      } as api.PhotoBrowsePage;
    });
    vi.stubGlobal("caches", { keys: async () => [], delete: async () => true });
    const cache = new PhotoPreviewCache("session", vi.fn());
    const read = vi.spyOn(cache, "get").mockImplementation(async (id) => new Blob([id]));
    vi.stubGlobal(
      "URL",
      class extends URL {
        static createObjectURL = vi
          .fn()
          .mockReturnValueOnce("blob:alex-a")
          .mockReturnValueOnce("blob:alex-b");
        static revokeObjectURL = vi.fn();
      },
    );
    const faces = new PhotoFaces("session", vi.fn());
    await faces.load("version-a");
    const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
    render(PhotoPeople, { faces, session: "session", cache, assetID: "photo-current" });
    await fireEvent.click(
      screen.getByRole("button", {
        name: `Find or create a person: ${assigned ? "Alex" : "Face 1"}`,
      }),
    );
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
    await screen.findByRole("option", { name: "Alex · 2 people" });
    expect(screen.queryByRole("option", { name: "Alex" })).toBeNull();
    await fireEvent.mouseDown(screen.getByRole("option", { name: "Alex · 2 people" }));
    const dialog = await screen.findByRole("dialog", { name: "Choose Alex" });
    expect((screen.getByRole("button", { name: "Name face" }) as HTMLButtonElement).disabled).toBe(
      !assigned,
    );
    if (assigned) {
      expect((dialog.querySelector('input[value="person-a"]') as HTMLInputElement).checked).toBe(
        true,
      );
      expect((dialog.querySelector('input[value="person-b"]') as HTMLInputElement).checked).toBe(
        false,
      );
      expect(write).not.toHaveBeenCalled();
    }
    await waitFor(() =>
      expect(screen.getAllByRole("img").map((image) => image.getAttribute("src"))).toEqual([
        "blob:alex-a",
        "blob:alex-b",
      ]),
    );
    expect(list).toHaveBeenCalledWith(
      expect.objectContaining({ hidden: false, page_size: 4 }),
      expect.objectContaining({ session: "session" }),
    );
    expect(read.mock.calls.map(([id]) => id)).toEqual(["photo-1", "photo-2"]);
    if (selection === "cancel")
      await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    else {
      const radio = dialog.querySelector(`input[value="${assigned ? "person-b" : selection}"]`)!;
      await fireEvent.click(radio);
      await fireEvent.click(screen.getByRole("button", { name: "Name face" }));
      await waitFor(() =>
        expect(write).toHaveBeenCalledWith(
          "version-a",
          expect.objectContaining(
            selection === "new" ? { person_name: "Alex" } : { person_id: "person-b" },
          ),
          expect.anything(),
        ),
      );
      expect(write.mock.calls[0]![1]).toMatchObject({ face_id: face.face_id, revision: 3 });
    }
    expect(screen.queryByRole("dialog")).toBeNull();
    if (assigned)
      expect(screen.getByRole("button", { name: "Find or create a person: Alex" })).toBeTruthy();
    expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:alex-a");
    expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:alex-b");
    if (selection === "cancel") expect(write).not.toHaveBeenCalled();
    await cache.dispose();
    faces.dispose();
  },
);

it("distinguishes namesakes with the same date and no photos by identity", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [face], assertions: [], people: [] });
  const search = vi.spyOn(api, "listPeople").mockResolvedValue({
    items: ["alex-a@example.test", "alex-b@example.test", ""].map((identity_label, index) => ({
      person_id: `person-${index}`,
      display_name: "Alex Smith",
      display_name_folded: "alex smith",
      created_at: "2026-01-01T00:00:00Z",
      state: "curated",
      revision: 1,
      identity_label,
    })),
    next_cursor: "different-name",
  });
  vi.spyOn(api, "listPhotoAssets").mockResolvedValue({
    items: [],
  } as unknown as api.PhotoBrowsePage);
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Alex Smith · 3 people" }));
  const dialog = await screen.findByRole("dialog", { name: "Choose Alex Smith" });
  expect(within(dialog).getByText("alex-a@example.test")).toBeTruthy();
  expect(
    within(dialog).getByText(new Date("2026-01-01T00:00:00Z").toLocaleDateString()),
  ).toBeTruthy();
  await fireEvent.click(within(dialog).getByLabelText(/alex-b@example.test/));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Name face" }));
  await waitFor(() =>
    expect(write).toHaveBeenCalledWith(
      "version-a",
      expect.objectContaining({ person_id: "person-1" }),
      expect.anything(),
    ),
  );
  expect(search.mock.calls.some(([params]) => params?.cursor)).toBe(false);
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
    target === "face"
      ? "You can't undo this here."
      : "You can't add a whole-photo mark back from the browser.",
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

it.each([
  { source: "list", result: "empty" },
  { source: "list", result: "failure" },
  { source: "list", result: "relock" },
  { source: "image", result: "failure" },
  { source: "image", result: "relock" },
])("handles Hidden same-name previews: $source $result", async ({ source, result }) => {
  const { HiddenAccess } = await import("./hiddenAccess.svelte.js");
  const { APIError } = await import("./api-transport.js");
  const lost = vi.fn();
  const access = new HiddenAccess("session", lost);
  const run = vi.spyOn(access, "run");
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [face], assertions: [], people: [] });
  vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [
      {
        person_id: "person-a",
        display_name: "Alex",
        state: "curated",
        revision: 1,
        display_name_folded: "alex",
        created_at: "2025-01-01T00:00:00Z",
      },
      {
        person_id: "person-b",
        display_name: "Alex",
        state: "curated",
        revision: 1,
        display_name_folded: "alex",
        created_at: "2026-01-01T00:00:00Z",
      },
    ],
  });
  const list = vi.spyOn(api, "listPhotoAssets").mockImplementation(async (request) => {
    if (source === "image") {
      const row = photo(request.query.filters!.person_ids![0] === "person-a" ? 1 : 2);
      row.previews.grid = { state: "ready", generation_id: `grid-${row.asset_id}` };
      return { items: [row] } as api.PhotoBrowsePage;
    }
    if (result === "failure") throw new Error("Preview read failed");
    if (result === "relock") throw new APIError("Hidden locked", 403, "photo_hidden_locked");
    return { items: [] } as unknown as api.PhotoBrowsePage;
  });
  const { PhotoPreviewCache } = await import("./photoPreviewCache.js");
  const cache = new PhotoPreviewCache("session", lost, true);
  let fail!: (cause: unknown) => void;
  let signal!: AbortSignal;
  if (source === "image") {
    vi.stubGlobal(
      "URL",
      class extends URL {
        static createObjectURL = vi.fn(() => "blob:alex");
        static revokeObjectURL = vi.fn();
      },
    );
    vi.spyOn(cache, "get")
      .mockResolvedValueOnce(new Blob(["synthetic"]))
      .mockImplementationOnce((_id, _generation, caller) => {
        signal = caller!;
        return new Promise((_resolve, reject) => {
          fail = reject;
        });
      });
  }
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", lost, true, vi.fn(), access);
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session", cache, assetID: "photo-current" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Alex · 2 people" }));
  await waitFor(() => expect(list).toHaveBeenCalled());
  expect(list).toHaveBeenCalledWith(
    expect.objectContaining({ hidden: true }),
    expect.objectContaining({ session: "session" }),
  );
  expect(run).toHaveBeenCalled();
  if (source === "image") {
    await screen.findByRole("img");
    await waitFor(() => expect(fail).toBeTypeOf("function"));
    fail(new APIError("Preview failed", result === "relock" ? 403 : 500, "preview_failed"));
  }
  if (result === "relock") {
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(lost).toHaveBeenCalledWith(expect.objectContaining({ status: 403 }));
    expect((source === "image" ? signal : list.mock.calls[0]![1]!.signal)!.aborted).toBe(true);
  } else {
    await screen.findAllByText(
      result === "empty" ? "No photos you can see." : "Preview unavailable",
    );
    expect(screen.getByText(new Date("2025-01-01T00:00:00Z").toLocaleDateString())).toBeTruthy();
    await fireEvent.click(screen.getAllByRole("radio")[1]!);
    await fireEvent.click(screen.getByRole("button", { name: "Name face" }));
    await waitFor(() =>
      expect(write).toHaveBeenCalledWith(
        "version-a",
        expect.objectContaining({ person_id: "person-b" }),
        expect.anything(),
      ),
    );
  }
  if (source === "image") {
    expect(signal.aborted).toBe(true);
    expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:alex");
  }
  await cache.dispose();
  faces.dispose();
});

it.each(["complete", "broad", "case"])("completes only the last name group: %s", async (result) => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({
    faces: [face],
    assertions: [],
    people: [],
  });
  const name = "Alex";
  const folded = "alex";
  const person = (index: number): api.PersonSummary => ({
    person_id: `person-${index}`,
    display_name: name,
    state: "curated",
    revision: 1,
    display_name_folded: folded,
    created_at: "2026-01-01T00:00:00Z",
  });
  const query = result === "broad" ? "Al" : name;
  let finish!: (page: api.PersonPage) => void;
  const search = vi.spyOn(api, "listPeople").mockImplementation(async ({ query, cursor } = {}) => {
    if (!query) return { items: [] };
    if (!cursor)
      return {
        items: Array.from({ length: 100 }, (_, i) => person(i)),
        next_cursor: "tail",
        same_name_continues: true,
      };
    return new Promise((resolve) => {
      finish = resolve;
    });
  });
  vi.spyOn(api, "listPhotoAssets").mockResolvedValue({
    items: [],
  } as unknown as api.PhotoBrowsePage);
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: query } });
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  expect(screen.queryByRole("option", { name: new RegExp(name) })).toBeNull();
  expect(screen.queryByRole("option", { name: `Create person "${query}"` })).toBeNull();
  finish({
    items: [
      ...(result === "case" ? [{ ...person(101), display_name: name.toUpperCase() }] : []),
      person(100),
      ...(result === "broad"
        ? [{ ...person(101), display_name: "Alice", display_name_folded: "alice" }]
        : []),
    ],
    next_cursor: result === "broad" ? "more" : undefined,
  });
  await fireEvent.mouseDown(
    await screen.findByRole("option", {
      name: `${name} · ${result === "case" ? 102 : 101} people`,
    }),
  );
  const dialog = await screen.findByRole("dialog", { name: `Choose ${name}` });
  await fireEvent.click(dialog.querySelector('input[value="person-100"]')!);
  await fireEvent.click(screen.getByRole("button", { name: "Name face" }));
  await waitFor(() =>
    expect(write).toHaveBeenCalledWith(
      "version-a",
      expect.objectContaining({ person_id: "person-100" }),
      expect.anything(),
    ),
  );
  expect(search.mock.calls.filter(([params]) => params?.query === query)).toHaveLength(2);
  expect(search).toHaveBeenCalledWith({ query, limit: 100, cursor: "tail" }, expect.anything());

  faces.dispose();
});

it("ignores an older continuation after a new search", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [face], assertions: [], people: [] });
  const person: api.PersonSummary = {
    person_id: "person-a",
    display_name: "Alex",
    state: "curated",
    revision: 1,
    display_name_folded: "alex",
    created_at: "2026-01-01T00:00:00Z",
  };
  let finish!: (page: api.PersonPage) => void;
  vi.spyOn(api, "listPeople").mockImplementation(async ({ query, cursor } = {}) => {
    if (cursor)
      return new Promise((resolve) => {
        finish = resolve;
      });
    return query === "Alex"
      ? { items: [person], next_cursor: "tail", same_name_continues: true }
      : { items: [{ ...person, display_name: "Bea", display_name_folded: "bea" }] };
  });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Bea" } });
  await screen.findByRole("option", { name: "Bea" });
  await act(() => finish({ items: [person] }));
  await waitFor(() => expect(screen.queryByRole("option", { name: /Alex/ })).toBeNull());
  expect(screen.getByRole("option", { name: "Bea" })).toBeTruthy();
  faces.dispose();
});

it.each([
  ["initial", 401],
  ["continuation", 401],
  ["initial", 500],
  ["continuation", 500],
] as const)("recovers session failure during %s search with status %s", async (page, status) => {
  const { APIError } = await import("./api-transport.js");
  const cause = new APIError("Search rejected", status, "search_rejected");
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [face], assertions: [], people: [] });
  vi.spyOn(api, "listPeople").mockImplementation(async ({ query, cursor } = {}) => {
    if (!query) return { items: [] };
    if (page === "continuation" && !cursor)
      return {
        items: [
          {
            person_id: "person-a",
            display_name: "Alex",
            display_name_folded: "alex",
            state: "curated",
            revision: 1,
            created_at: "2026-01-01T00:00:00Z",
          },
        ],
        next_cursor: "tail",
        same_name_continues: true,
      };
    throw cause;
  });
  const lost = vi.fn();
  const faces = new PhotoFaces("session", lost);
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
  await screen.findByText("Search rejected");
  if (status === 401) expect(lost).toHaveBeenCalledExactlyOnceWith(cause);
  else expect(lost).not.toHaveBeenCalled();
  expect(screen.queryByRole("option", { name: /Alex/ })).toBeNull();
  faces.dispose();
});
