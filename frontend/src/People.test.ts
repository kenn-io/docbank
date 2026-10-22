import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PeopleView from "./People.svelte";
import { People } from "./people.svelte.js";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { ada, grace } from "./people-test-fixtures.js";
import { photoFace, photoPersonAssertion } from "./photo-test-fixtures.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
      unobserve() {}
    },
  );
});
const unlockedAccess = () => {
  const access = new HiddenAccess("session", vi.fn());
  access.acceptState({ configured: true, expires_in: 60, expires_at: "later" }, Date.now());
  return access;
};
it("opens Library and unlocked Hidden counts, including cards without photo evidence", async () => {
  const access = unlockedAccess();
  const people = new People("session", vi.fn(), access);
  people.items = [ada, grace];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const open = vi.fn();
  const view = render(PeopleView, { people, cache, onperson: open });
  await fireEvent.click(screen.getByRole("button", { name: "Ada 3 Library photos" }));
  expect(open).toHaveBeenCalledWith("ada", "Ada");
  await fireEvent.click(screen.getByRole("button", { name: "1 Hidden photo" }));
  expect(open).toHaveBeenCalledWith("ada", "Ada", true);
  expect(screen.getByRole("button", { name: "Grace 0 Library photos" })).toBeTruthy();
  await act(() => access.acceptState({ configured: true }, Date.now() + 1));
  expect(screen.queryByRole("button", { name: "1 Hidden photo" })).toBeNull();
  view.unmount();
  people.dispose();
  access.dispose();
  await cache.dispose();
});
it("keeps the search text with the applied query when the view returns", async () => {
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada] });
  const people = new People("session", vi.fn());
  const cache = new PhotoPreviewCache("session", vi.fn());
  const open = vi.fn();
  let view = render(PeopleView, { people, cache, onperson: open });
  await fireEvent.input(screen.getByRole("textbox", { name: "Find people" }), {
    target: { value: "Ad" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Ada 3 Library photos" }));
  expect(open).toHaveBeenCalledWith("ada", "Ada");
  view.unmount();
  people.conceal();
  view = render(PeopleView, { people, cache, onperson: open });
  await people.load();
  expect(read.mock.calls.at(-1)![0]).toMatchObject({ query: "Ad" });
  expect((screen.getByRole("textbox", { name: "Find people" }) as HTMLInputElement).value).toBe(
    "Ad",
  );
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("renames a zero-photo person", async () => {
  const people = new People("session", vi.fn());
  people.items = [grace];
  vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [{ ...grace, display_name: "Grace Hopper", revision: 5 }],
  });
  const rename = vi.spyOn(api, "renamePerson").mockResolvedValue({
    ...grace,
    display_name: "Grace Hopper",
    revision: 5,
    origin: "operator",
    updated_at: "2026-01-02",
  });
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Rename" }));
  const dialog = screen.getByRole("dialog", { name: "Rename Grace" });
  await fireEvent.input(within(dialog).getByRole("textbox", { name: "Person name" }), {
    target: { value: "Grace Hopper" },
  });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(rename).toHaveBeenCalledOnce();
  expect(screen.getByRole("button", { name: "Grace Hopper 0 Library photos" })).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("chooses the merge survivor by ID and requires confirmation", async () => {
  const people = new People("session", vi.fn());
  people.items = [ada];
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada, grace] });
  const merge = vi.spyOn(api, "mergePerson").mockResolvedValue({} as api.PersonMergeReceipt);
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge Ada" });
  await fireEvent.click(within(dialog).getByRole("button", { name: /Find the person to keep/ }));
  await waitFor(() =>
    expect(
      screen.getByRole("option", {
        name: `Grace · ${new Date(grace.created_at).toLocaleDateString()}`,
      }),
    ).toBeTruthy(),
  );
  await fireEvent.mouseDown(
    screen.getByRole("option", {
      name: `Grace · ${new Date(grace.created_at).toLocaleDateString()}`,
    }),
  );
  const confirm = within(dialog).getByRole("button", { name: "Merge" });
  expect(confirm.hasAttribute("disabled")).toBe(true);
  await fireEvent.click(within(dialog).getByRole("checkbox", { name: "Confirm merge" }));
  await fireEvent.click(confirm);
  await waitFor(() => expect(merge).toHaveBeenCalled());
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("clears the exact retained face in a merge conflict, then asks for a fresh confirmation", async () => {
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [grace] });
  const faces = vi.spyOn(api, "listPhotoFaces").mockResolvedValue({
    faces: [
      photoFace({
        face_id: "conflict",
        content_version_id: "old-version",
        rejected_person_ids: ["ada"],
      }),
    ],
    assertions: [photoPersonAssertion({ content_version_id: "old-version", person_id: "grace" })],
    people: [
      { person_id: "grace", display_name: "Grace", state: "curated" },
      { person_id: "ada", display_name: "Ada", state: "curated" },
    ],
  });
  const decide = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const merge = vi
    .spyOn(api, "mergePerson")
    .mockRejectedValueOnce(
      new APIError(
        "Review faces",
        409,
        "person_merge_conflict",
        undefined,
        [],
        [{ face_id: "conflict", version: "old-version" }],
      ),
    )
    .mockResolvedValue({} as api.PersonMergeReceipt);
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getAllByRole("button", { name: "Merge" })[0]!);
  const dialog = screen.getByRole("dialog", { name: "Merge Ada" });
  await fireEvent.click(within(dialog).getByRole("button", { name: /Find the person to keep/ }));
  await fireEvent.mouseDown(
    await screen.findByRole("option", {
      name: `Grace · ${new Date(grace.created_at).toLocaleDateString()}`,
    }),
  );
  await fireEvent.click(within(dialog).getByRole("checkbox", { name: "Confirm merge" }));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Merge" }));
  const faceRow = await within(dialog).findByText("Face 1 · Grace · marked not Ada");
  expect(faces).toHaveBeenCalledWith("old-version", expect.anything());
  await fireEvent.click(within(faceRow.parentElement!).getByRole("button", { name: "Clear name" }));
  await fireEvent.click(
    within(screen.getByRole("dialog", { name: "Clear the name Grace from Face 1?" })).getByRole(
      "button",
      { name: "Clear name" },
    ),
  );
  await waitFor(() =>
    expect(within(dialog).queryByRole("list", { name: "Faces to review" })).toBeNull(),
  );
  expect(decide).toHaveBeenCalledWith(
    "old-version",
    expect.objectContaining({ action: "retract", face_id: "conflict", revision: 7 }),
    expect.anything(),
  );
  const confirm = within(dialog).getByRole("checkbox", {
    name: "Confirm merge",
  }) as HTMLInputElement;
  expect(confirm.checked).toBe(false);
  expect(within(dialog).getByRole("button", { name: "Merge" }).hasAttribute("disabled")).toBe(true);
  await fireEvent.click(confirm);
  await fireEvent.click(within(dialog).getByRole("button", { name: "Merge" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(merge).toHaveBeenCalledTimes(2);
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("clears an open merge search and its late response when Hidden locks", async () => {
  const access = unlockedAccess();
  const people = new People("session", vi.fn(), access);
  people.items = [ada];
  let finish!: (page: api.PersonPage) => void;
  const read = vi.spyOn(api, "listPeople").mockImplementation(
    () =>
      new Promise((resolve) => {
        finish = resolve;
      }),
  );
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  await waitFor(() => expect(read).toHaveBeenCalled());
  await act(() => access.acceptState({ configured: true }, Date.now() + 1));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(read.mock.calls[0]![1]!.signal!.aborted).toBe(true);
  finish({ items: [grace] });
  await act(() => Promise.resolve());
  expect(screen.queryByText("Grace")).toBeNull();
  view.unmount();
  people.dispose();
  access.dispose();
  await cache.dispose();
});
it("keeps portraits through in-place updates and releases them when cards clear", async () => {
  vi.stubGlobal(
    "Image",
    class {
      src = "";
      naturalWidth = 1000;
      naturalHeight = 800;
      decode() {
        return Promise.resolve();
      }
    },
  );
  vi.stubGlobal(
    "URL",
    Object.assign(URL, { createObjectURL: vi.fn(() => "blob:person"), revokeObjectURL: vi.fn() }),
  );
  const people = new People("session", vi.fn());
  people.items = [
    {
      ...ada,
      photo_summary: {
        library_count: 1,
        face: {
          asset_id: "asset",
          content_version_id: "version",
          generation_id: "ready",
          rotation: 0,
          box: { x: 0.1, y: 0.1, width: 0.2, height: 0.2 },
        },
      },
    },
  ];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const get = vi.spyOn(cache, "get").mockResolvedValue(new Blob(["synthetic"]));
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await waitFor(() => expect(screen.getByRole("img", { name: "Face of Ada" })).toBeTruthy());
  expect(get.mock.calls[0]![4]).toBe("ada");
  // An in-place card update with the same face keeps its portrait.
  await act(() => (people.items = [{ ...people.items[0]!, display_name: "Ada L", revision: 3 }]));
  expect(screen.getByRole("img", { name: "Face of Ada L" })).toBeTruthy();
  expect(get).toHaveBeenCalledOnce();
  await act(() => people.conceal());
  expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:person");
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("disambiguates namesakes in merge options, selection, confirmation, and stale notice", async () => {
  const source = { ...ada, display_name: "Alex", identity_label: "alex@example.test" };
  const survivor = { ...grace, display_name: "Alex" };
  const detail = new Date(survivor.created_at).toLocaleDateString();
  const people = new People("session", vi.fn());
  people.items = [source];
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [survivor] });
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  await fireEvent.click(screen.getByRole("button", { name: /Find the person to keep/ }));
  await fireEvent.mouseDown(await screen.findByRole("option", { name: `Alex · ${detail}` }));
  read.mockResolvedValue({ items: [] });
  await fireEvent.click(screen.getByRole("button", { name: /Alex ·/ }));
  const search = screen.getByRole("combobox", { name: "Find the person to keep" });
  await fireEvent.input(search, { target: { value: "Other" } });
  await waitFor(() => expect(read.mock.calls.at(-1)![0]).toMatchObject({ query: "Other" }));
  await fireEvent.keyDown(search, { key: "Escape" });
  expect(screen.getByRole("button", { name: /Alex ·/ }).textContent).toContain(`Alex · ${detail}`);
  expect(
    screen.getByText(
      `Keep Alex · ${detail}. Move Alex · alex@example.test's records into this person.`,
    ),
  ).toBeTruthy();
  await act(() => {
    people.merge = { ...people.merge!, notice: "stale" };
  });
  expect(screen.getByRole("status").textContent).toContain(
    `Alex · alex@example.test or Alex · ${detail} changed.`,
  );
  view.unmount();
  people.dispose();
  await cache.dispose();
});
