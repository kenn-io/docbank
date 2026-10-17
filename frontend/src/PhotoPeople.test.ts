import { photo, photoFace, photoPersonAssertion, photoFacePage } from "./photo-test-fixtures.js";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { PhotoFaces } from "./photoFaces.svelte.js";
import { Photos } from "./photos.svelte.js";
import PhotoPeople from "./PhotoPeople.svelte";
import * as api from "./generated/docbank.js";
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
beforeEach(() => {
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
it("explains that removed faces keep their place in the photo limit", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [face], assertions: [], people: [] });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  expect(screen.getByRole("button", { name: "Remove face" })).toBeTruthy();
  expect(
    screen.getByText("Removed faces still count toward this photo's 1,000-face limit."),
  ).toBeTruthy();
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
    { query: normalized, limit: 100 },
    expect.objectContaining({ session: "session" }),
  );
  await fireEvent.keyDown(screen.getByRole("combobox"), { key: "Escape" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await waitFor(() =>
    expect(search).toHaveBeenLastCalledWith(
      { query: "", limit: 100 },
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
          created_at: "2026-01-01T00:00:00Z",
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
    vi.mocked(api.listPhotoFaces).mockImplementation(async (version) =>
      version === "version-1"
        ? {
            faces: [
              photoFace({
                face_id: "left",
                assertion_id: "left",
                box: { x: 0.1, y: 0.2, width: 0.1, height: 0.1 },
              }),
              photoFace({
                face_id: "right",
                assertion_id: "right",
                box: { x: 0.7, y: 0.2, width: 0.1, height: 0.1 },
              }),
              photoFace({
                face_id: "withdrawn",
                assertion_id: "old",
                box: { x: 0, y: 0, width: 1, height: 1 },
              }),
              photoFace({
                face_id: "small",
                assertion_id: "left",
                box: { x: 0, y: 0, width: 0.05, height: 0.05 },
              }),
              photoFace({
                face_id: "z-tied",
                assertion_id: "left",
                box: { x: 0.5, y: 0.2, width: 0.1, height: 0.1 },
              }),
              photoFace({
                face_id: "dismissed",
                assertion_id: "left",
                dismissed: true,
                box: { x: 0, y: 0, width: 1, height: 1 },
              }),
            ],
            assertions: [
              photoPersonAssertion({ assertion_id: "left" }),
              photoPersonAssertion({ assertion_id: "right", person_id: "person-b" }),
              photoPersonAssertion({ assertion_id: "old", action: "retract" }),
            ],
            people: [],
          }
        : {
            faces: [assigned ? { ...face, assertion_id: "assertion-a" } : face],
            assertions: assigned ? [photoPersonAssertion()] : [],
            people: assigned
              ? [{ person_id: "person-a", display_name: "Alex", state: "curated" }]
              : [],
          },
    );
    const list = vi.spyOn(api, "listPhotoAssets").mockImplementation(async (request) => {
      expect(request.query.filters!.person_ids![0]).toMatch(/^person-[ab]$/);
      const row = photo(1);
      row.rotation = 90;
      row.previews.grid = { state: "ready", generation_id: "grid-1" };
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
    expect(screen.getAllByRole("img").map((image) => image.style.transform)).toEqual([
      "translate(-50%,-50%) rotate(90deg)",
      "translate(-50%,-50%) rotate(90deg)",
    ]);
    expect(list).toHaveBeenCalledWith(
      expect.objectContaining({ hidden: false, page_size: 4 }),
      expect.objectContaining({ session: "session" }),
    );
    expect(read.mock.calls.map(([id]) => id)).toEqual(["photo-1", "photo-1"]);
    expect(api.listPhotoFaces).toHaveBeenCalledWith(
      "version-1",
      expect.objectContaining({ session: "session", signal: expect.any(AbortSignal) }),
    );
    const images = screen.getAllByRole("img");
    expect(parseFloat(images[0]!.style.left)).toBeCloseTo(-48);
    expect(parseFloat(images[1]!.style.left)).toBeCloseTo(-48);
    expect(parseFloat(images[0]!.style.top)).toBeCloseTo(172);
    expect(parseFloat(images[1]!.style.top)).toBeCloseTo(-68);
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

it.each(
  ["select", "cancel", "scope"].flatMap((mode) =>
    ["list", "faces"].map((source) => ({ mode, source })),
  ),
)("bounds namesake preview reads within Hidden access: $mode $source", async ({ mode, source }) => {
  const { HiddenAccess } = await import("./hiddenAccess.svelte.js");
  const access = new HiddenAccess("session", vi.fn());
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
    })),
  });
  let active = 0;
  let maximum = 0;
  const pending = new Map<string, { finish: (fail?: boolean) => void; signal: AbortSignal }>();
  const pendingRead = (id: string, options?: { signal?: AbortSignal | null }) => {
    active++;
    maximum = Math.max(maximum, active);
    return new Promise<api.PhotoBrowsePage & api.PhotoFacePage>((resolve, reject) => {
      pending.set(id, {
        signal: options!.signal!,
        finish(fail) {
          active--;
          pending.delete(id);
          if (fail) reject(new Error("Preview read failed"));
          else
            resolve({
              items: [],
              facets: [],
              total: 0,
              relevance_available: false,
              faces: [],
              assertions: [],
              people: [],
            });
        },
      });
    });
  };
  const list = vi.spyOn(api, "listPhotoAssets").mockImplementation(async (request, options) => {
    const id = request.query.filters!.person_ids![0]!;
    expect(request.hidden).toBe(true);
    return source === "list"
      ? pendingRead(id, options)
      : ({ items: [{ ...photo(1), content_version_id: id }] } as api.PhotoBrowsePage);
  });
  const lockRequest = vi.spyOn(api, "lockPhotoHidden").mockResolvedValue({ configured: true });
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn(), true, vi.fn(), access);
  await faces.load("version-a");
  const { PhotoPreviewCache } = await import("./photoPreviewCache.js");
  const cache = new PhotoPreviewCache("session", vi.fn());
  if (source === "faces") vi.mocked(api.listPhotoFaces).mockImplementation(pendingRead);
  render(PhotoPeople, { faces, session: "session", cache });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
  const group = await screen.findByRole("option", { name: "Alex · 6 people" });
  await fireEvent.mouseDown(group);
  await waitFor(() => expect(list).toHaveBeenCalledTimes(4));
  expect(maximum).toBe(4);
  if (mode === "select") {
    await act(() => pending.get("person-0")!.finish(true));
    await screen.findByText("Preview unavailable");
    await waitFor(() => expect(list).toHaveBeenCalledTimes(5));
    await act(() => pending.get("person-1")!.finish());
    await waitFor(() => expect(list).toHaveBeenCalledTimes(6));
    await waitFor(() => expect(pending.size).toBe(4));
    await act(async () => {
      for (const read of pending.values()) read.finish();
      await access.settled();
    });
    expect(maximum).toBe(4);
  } else {
    const lock = access.lock();
    const reads = [...pending.values()];
    if (mode === "cancel") await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    else await act(() => access.acceptState({ configured: true }, Date.now() + 1));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(reads.every((read) => read.signal.aborted)).toBe(true);
    await act(() => reads[0]!.finish(true));
    expect(lockRequest).not.toHaveBeenCalled();
    await act(async () => {
      for (const read of reads.slice(1)) read.finish();
      await lock;
    });
    expect(lockRequest).toHaveBeenCalledOnce();
    expect(list).toHaveBeenCalledTimes(4);
    expect(write).not.toHaveBeenCalled();
  }
  await cache.dispose();
  access.dispose();
  faces.dispose();
});

it.each([
  {
    label: "distinct identities",
    identity: "alex-a@example.test",
    secondIdentity: "alex-b@example.test",
    secondTime: "12:00:00",
  },
  { label: "same displayed date", identity: "", secondTime: "12:00:30" },
  { label: "shared identity", identity: "alex@example.test", secondTime: "13:00:00" },
])("distinguishes namesakes for $label", async ({ identity, secondIdentity, secondTime }) => {
  const ids = ["person-alex-a", "person-alex-b", "person-alex-c"];
  const identities = [
    identity,
    secondIdentity ?? identity,
    secondIdentity ? "" : "other@example.test",
  ];
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [face], assertions: [], people: [] });
  const search = vi.spyOn(api, "listPeople").mockResolvedValue({
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
  vi.spyOn(api, "listPhotoAssets").mockResolvedValue({
    items: [],
  } as unknown as api.PhotoBrowsePage);
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Alex · 3 people" }));
  const dialog = within(await screen.findByRole("dialog", { name: "Choose Alex" }));
  if (secondIdentity) {
    expect(dialog.getByText("alex-a@example.test")).toBeTruthy();
    expect(dialog.getByText(new Date("2026-01-01T12:00:00Z").toLocaleDateString())).toBeTruthy();
    expect(dialog.queryByText("person-alex-a")).toBeNull();
  } else {
    expect(dialog.getByText("person-alex-a")).toBeTruthy();
    expect(dialog.getByText("other@example.test")).toBeTruthy();
  }
  expect(dialog.queryByText("person-alex-c")).toBeNull();
  await fireEvent.click(
    dialog.getByRole("radio", { name: secondIdentity ? /alex-b@example.test/ : /person-alex-b/ }),
  );
  await fireEvent.click(dialog.getByRole("button", { name: "Name face" }));
  await waitFor(() =>
    expect(write).toHaveBeenCalledWith(
      "version-a",
      expect.objectContaining({ person_id: "person-alex-b" }),
      expect.anything(),
    ),
  );
  expect(search.mock.calls.some(([params]) => params?.cursor)).toBe(false);
  faces.dispose();
});

it.each(
  ["face", "mark"].flatMap((target) => [false, true].map((pending) => ({ target, pending }))),
)(
  "confirms $target removal with the captured revision, pending=$pending",
  async ({ target, pending }) => {
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
    let finish!: (page: api.PhotoFacePage) => void;
    let reading: Promise<void> | undefined;
    if (pending) {
      vi.mocked(api.listPhotoFaces).mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            finish = resolve;
          }),
      );
      await act(() => {
        reading = faces.load("version-a");
      });
    }
    faces.faces = faces.faces.map((row) => ({ ...row, revision: 90 }));
    faces.assertions = faces.assertions.map((row) => ({ ...row, revision: 90 }));
    await fireEvent.click(within(dialog).getByRole("button", { name: label }));
    if (pending) {
      await screen.findByText("People are reloading; try again.");
      expect(write).not.toHaveBeenCalled();
      await act(async () => {
        finish(photoFacePage());
        await reading;
      });
      faces.dispose();
      return;
    }
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
  },
);

it.each([
  { source: "list", result: "empty" },
  { source: "list", result: "relock" },
  { source: "image", result: "failure" },
  { source: "faces", result: "failure" },
  { source: "faces", result: "relock" },
  { source: "image", result: "relock" },
])("handles Hidden same-name previews: $source $result", async ({ source, result }) => {
  const { HiddenAccess } = await import("./hiddenAccess.svelte.js");
  const { APIError } = await import("./api-transport.js");
  const lost = vi.fn();
  const access = new HiddenAccess("session", lost);
  const run = vi.spyOn(access, "run");
  vi.spyOn(api, "listPhotoFaces").mockImplementation(async (version) => {
    if (version !== "version-a" && source === "faces")
      throw new APIError("Face read failed", result === "relock" ? 403 : 500, "face_read_failed");
    return {
      faces: [face],
      assertions:
        version === "version-a"
          ? []
          : [
              photoPersonAssertion({
                person_id: version === "version-1" ? "person-a" : "person-b",
                photo_mark: true,
              }),
            ],
      people: [],
    };
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
    if (source === "image" || source === "faces") {
      const row = photo(request.query.filters!.person_ids![0] === "person-a" ? 1 : 2);
      row.previews.grid = { state: "ready", generation_id: `grid-${row.asset_id}` };
      return { items: [row] } as api.PhotoBrowsePage;
    }
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

it("keeps 100 loaded people and retries a later page to find an exact person", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [face], assertions: [], people: [] });
  const existing: api.PersonSummary = {
    person_id: "person-exact",
    display_name: "Alex",
    display_name_folded: "alex",
    state: "curated",
    revision: 1,
    created_at: "2026-01-01T00:00:00Z",
  };
  let finish!: (page: api.PersonPage) => void;
  let attempts = 0;
  const search = vi.spyOn(api, "listPeople").mockImplementation(async ({ query, cursor } = {}) => {
    if (!query) return { items: [] };
    if (!cursor)
      return {
        items: Array.from({ length: 100 }, (_, index) => ({
          ...existing,
          person_id: `person-${index}`,
          display_name: `Aaron ${index} Alex`,
          display_name_folded: `aaron ${index} alex`,
        })),
        next_cursor: "later-names",
        same_name_continues: false,
      };
    expect(cursor).toBe("later-names");
    if (++attempts === 1) throw new Error("People search unavailable");
    return new Promise((resolve) => {
      finish = resolve;
    });
  });
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
  const more = await screen.findByRole("option", { name: "Load more" });
  expect(search.mock.calls.some(([params]) => params?.cursor)).toBe(false);
  expect(screen.getByRole("option", { name: 'Create person "Alex"' })).toBeTruthy();
  await fireEvent.mouseDown(more);
  await screen.findByText("People search unavailable");
  expect(screen.getByRole("option", { name: "Aaron 0 Alex" })).toBeTruthy();
  expect(screen.queryByRole("option", { name: 'Create person "Alex"' })).toBeNull();
  await fireEvent.mouseDown(screen.getByRole("option", { name: "Load more" }));
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  expect(screen.queryByRole("option", { name: 'Create person "Alex"' })).toBeNull();
  await act(() => finish({ items: [existing] }));
  expect(await screen.findByRole("option", { name: 'Create person "Alex"' })).toBeTruthy();
  expect(screen.queryByText("People search unavailable")).toBeNull();
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Alex" }));
  await waitFor(() =>
    expect(write).toHaveBeenCalledWith(
      "version-a",
      expect.objectContaining({ person_id: "person-exact" }),
      expect.anything(),
    ),
  );
  faces.dispose();
});

it.each(["complete", "broad", "case"])("completes paginated name groups: %s", async (result) => {
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

it("ignores an old search response after Hidden expiry", async () => {
  const { HiddenAccess } = await import("./hiddenAccess.svelte.js");
  const access = new HiddenAccess("session", vi.fn());
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    access.acceptState({ configured: true, expires_in: 60, change_id: "same" }, Date.now());
    const person = {
      person_id: "person-a",
      display_name: "Alex",
      display_name_folded: "alex",
      state: "curated" as const,
      revision: 1,
      created_at: "2026-01-01T00:00:00Z",
    };
    const search = vi
      .spyOn(api, "listPeople")
      .mockResolvedValue({ items: [person, { ...person, person_id: "person-b" }] });
    const faces = new Photos("session", vi.fn(), false, undefined, { hiddenAccess: access })
      .faceEditor;
    faces.version = "version-a";
    faces.faces = [face];
    render(PhotoPeople, { faces, session: "session" });
    await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Face 1" }));
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex" } });
    await screen.findByRole("option", { name: "Alex · 2 people" });
    let finishSearch!: (page: api.PersonPage) => void;
    search.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finishSearch = resolve;
        }),
    );
    await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Alex late" } });
    await waitFor(() => expect(finishSearch).toBeTypeOf("function"));
    await act(() => vi.advanceTimersByTimeAsync(60_001));
    await act(async () => finishSearch({ items: [person] }));
    expect(screen.queryByRole("option", { name: "Alex" })).toBeNull();
  } finally {
    access.dispose();
    vi.useRealTimers();
  }
});
