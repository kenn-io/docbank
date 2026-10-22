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
  vi.spyOn(api, "listPersonFacePreviews").mockResolvedValue({ items: [] });
});
const unlockedAccess = () => {
  const access = new HiddenAccess("session", vi.fn());
  access.acceptState({ configured: true, expires_in: 60, expires_at: "later" }, Date.now());
  return access;
};
it("starts People paging over when a saved cursor expires", async () => {
  const read = vi
    .spyOn(api, "listPeople")
    .mockResolvedValueOnce({ items: [ada], next_cursor: "stale" })
    .mockRejectedValueOnce(new APIError("invalid people cursor", 422, "validation"))
    .mockResolvedValueOnce({ items: [grace] });
  const people = new People("session", vi.fn());
  await people.load();
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Load more people" }));
  await screen.findByText("invalid people cursor");
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(people.items).toEqual([grace]));
  expect(read.mock.calls[2]![0]).not.toHaveProperty("cursor");
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("opens Library and unlocked Hidden counts, including cards without photo evidence", async () => {
  const access = unlockedAccess();
  const people = new People("session", vi.fn(), access);
  people.items = [{ ...ada, photo_summary: { library_count: 3, hidden_count: 1 } }, grace];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const open = vi.fn();
  const view = render(PeopleView, { people, cache, onperson: open });
  await fireEvent.click(screen.getByRole("button", { name: "Ada 3 Library photos" }));
  expect(open).toHaveBeenCalledWith("ada", "Ada");
  await fireEvent.click(screen.getByRole("button", { name: "1 Hidden photo" }));
  expect(open).toHaveBeenCalledWith("ada", "Ada", true);
  expect(screen.getByRole("button", { name: "Grace 0 Library photos" })).toBeTruthy();
  await act(() => {
    people.conceal();
    access.acceptState({ configured: true }, Date.now() + 1);
    people.items = [ada, grace];
  });
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
it("merges two selected cards after choosing the one to keep and confirming", async () => {
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [grace] });
  const merge = vi.spyOn(api, "mergePerson").mockResolvedValue({} as api.PersonMergeReceipt);
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Ada" }));
  expect(screen.getByRole("button", { name: "Merge" }).hasAttribute("disabled")).toBe(true);
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Grace" }));
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge people" });
  // Both cards show their Library evidence side by side.
  expect(within(dialog).getByText("3 Library photos")).toBeTruthy();
  expect(within(dialog).getByText("0 Library photos")).toBeTruthy();
  expect(within(dialog).queryByRole("checkbox", { name: "Confirm merge" })).toBeNull();
  const detail = new Date(grace.created_at).toLocaleDateString();
  await fireEvent.click(within(dialog).getByRole("radio", { name: `Keep Grace · ${detail}` }));
  const confirm = within(dialog).getByRole("button", { name: "Merge" });
  expect(confirm.hasAttribute("disabled")).toBe(true);
  await fireEvent.click(within(dialog).getByRole("checkbox", { name: "Confirm merge" }));
  await fireEvent.click(confirm);
  await waitFor(() => expect(merge).toHaveBeenCalledOnce());
  expect(merge.mock.calls[0]![0]).toBe("grace");
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("withdraws the blocking rejection on the exact retained face after confirmation, then asks again", async () => {
  const people = new People("session", vi.fn());
  const faces = vi.spyOn(api, "listPhotoFaces").mockResolvedValue({
    faces: [
      photoFace({
        face_id: "conflict",
        content_version_id: "old-version",
        assertion_id: "assigned",
        rejected_person_ids: ["grace", "other"],
      }),
    ],
    assertions: [
      photoPersonAssertion({
        assertion_id: "assigned",
        content_version_id: "old-version",
        person_id: "ada",
        action: "assert",
      }),
    ],
    // The faces endpoint lists depicted people; rejected IDs are not people rows.
    people: [{ person_id: "ada", display_name: "Ada", state: "curated" }],
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
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada] });
  const listPreviews = vi.spyOn(api, "listPersonFacePreviews").mockResolvedValue({
    items: [
      {
        asset_id: "conflict-photo",
        content_version_id: "old-version",
        generation_id: "ready",
        rotation: 0,
      },
    ],
  });
  const cache = new PhotoPreviewCache("session", vi.fn());
  vi.spyOn(cache, "get").mockResolvedValue(new Blob(["synthetic"]));
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
    "URL",
    class extends URL {
      static createObjectURL = vi.fn(() => "blob:conflict");
      static revokeObjectURL = vi.fn();
    },
  );
  people.items = [ada, grace];
  const view = render(PeopleView, {
    people,
    cache,
    onperson: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Ada" }));
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Grace" }));
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge people" });
  await fireEvent.click(within(dialog).getAllByRole("radio")[0]!);
  await fireEvent.click(within(dialog).getByRole("checkbox", { name: "Confirm merge" }));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Merge" }));
  const withdraw = await within(dialog).findByRole("button", {
    name: "Withdraw rejection for Grace",
  });
  expect(within(dialog).queryByRole("alert")).toBeNull();
  expect(
    await within(dialog).findByRole("img", { name: "Face 1 from Ada, rejected as Grace" }),
  ).toBeTruthy();
  expect(within(dialog).getByText("Face 1 from Ada · rejected as Grace")).toBeTruthy();
  expect(listPreviews).toHaveBeenCalledWith(
    "ada",
    expect.objectContaining({
      hidden: false,
      content_version_id: "old-version",
      face_id: "conflict",
    }),
    expect.objectContaining({ session: "session" }),
  );
  expect(faces).toHaveBeenCalledWith("old-version", expect.anything());
  // A rejection of someone outside this merge does not block it.
  expect(within(dialog).queryByRole("button", { name: "Withdraw rejection for Other" })).toBeNull();
  await fireEvent.click(withdraw);
  await fireEvent.click(
    within(
      screen.getByRole("dialog", {
        name: "Withdraw the rejection of Grace from Face 1? The face keeps its current name.",
      }),
    ).getByRole("button", { name: "Cancel" }),
  );
  expect(decide).not.toHaveBeenCalled();
  expect(within(dialog).getByRole("list", { name: "Faces to review" })).toBeTruthy();
  await fireEvent.click(withdraw);
  await fireEvent.click(
    within(
      screen.getByRole("dialog", {
        name: "Withdraw the rejection of Grace from Face 1? The face keeps its current name.",
      }),
    ).getByRole("button", { name: "Withdraw rejection" }),
  );
  await waitFor(() =>
    expect(within(dialog).queryByRole("list", { name: "Faces to review" })).toBeNull(),
  );
  expect(decide).toHaveBeenCalledOnce();
  expect(decide.mock.calls[0]![0]).toBe("old-version");
  expect(decide.mock.calls[0]![1]).toMatchObject({
    action: "withdraw_rejection",
    face_id: "conflict",
    revision: 7,
    person_id: "grace",
  });
  const confirm = within(dialog).getByRole("checkbox", {
    name: "Confirm merge",
  }) as HTMLInputElement;
  expect(confirm.checked).toBe(false);
  await fireEvent.click(confirm);
  await fireEvent.click(within(dialog).getByRole("button", { name: "Merge" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(merge).toHaveBeenCalledTimes(2);
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("clears the selected cards and open merge when People is concealed", async () => {
  const access = unlockedAccess();
  const people = new People("session", vi.fn(), access);
  people.items = [{ ...ada, photo_summary: { library_count: 3, hidden_count: 1 } }, grace];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Ada" }));
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Grace" }));
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge people" });
  expect(within(dialog).getByText("1 Hidden photo")).toBeTruthy();
  await act(() => {
    people.conceal();
    access.acceptState({ configured: true }, Date.now() + 1);
  });
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(screen.queryByRole("region", { name: "Selected people" })).toBeNull();
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
it("labels namesakes by detail, and by ID only when the details match", async () => {
  const same = { ...grace, display_name: "Alex", display_name_folded: "alex" };
  const people = new People("session", vi.fn());
  people.items = [
    { ...same, person_id: "alex-1" },
    { ...same, person_id: "alex-2" },
    { ...same, person_id: "alex-3", identity_label: "alex@example.org" },
  ];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  const detail = new Date(same.created_at).toLocaleDateString();
  const checkboxes = screen.getAllByRole("checkbox", { name: "Select Alex" });
  await fireEvent.click(checkboxes[0]!);
  await fireEvent.click(checkboxes[1]!);
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  let dialog = screen.getByRole("dialog", { name: "Merge people" });
  await fireEvent.click(
    within(dialog).getByRole("radio", { name: `Keep Alex · ${detail} · alex-2` }),
  );
  expect(
    within(dialog).getByText(
      `Keep Alex · ${detail} · alex-2. Move Alex · ${detail} · alex-1's records into this person.`,
    ),
  ).toBeTruthy();
  await fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await fireEvent.click(checkboxes[1]!);
  await fireEvent.click(screen.getAllByRole("checkbox", { name: "Select Alex" })[2]!);
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  dialog = screen.getByRole("dialog", { name: "Merge people" });
  expect(within(dialog).getByRole("radio", { name: `Keep Alex · ${detail}` })).toBeTruthy();
  expect(within(dialog).getByRole("radio", { name: "Keep Alex · alex@example.org" })).toBeTruthy();
  await act(() => {
    people.merge = { ...people.merge!, notice: "stale" };
  });
  expect(within(dialog).getByRole("status").textContent).toContain(
    `Alex · ${detail} or Alex · alex@example.org changed.`,
  );
  view.unmount();
  people.dispose();
  await cache.dispose();
});
