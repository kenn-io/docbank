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
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(merge.mock.calls[0]![0]).toBe("grace");
  expect(merge.mock.calls[0]![1]).toMatchObject({
    absorbed_person_id: "ada",
    absorbed_revision: 2,
  });
  expect(screen.queryByRole("button", { name: "Ada 3 Library photos" })).toBeNull();
  expect(screen.queryByRole("region", { name: "Selected people" })).toBeNull();
  view.unmount();
  people.dispose();
  await cache.dispose();
});
const conflictFaces = (assigned: "assert" | "withdrawn") => ({
  faces: [
    photoFace({
      face_id: "conflict",
      content_version_id: "old-version",
      rejected_person_ids: ["grace", "other"],
    }),
  ],
  assertions: [
    photoPersonAssertion({ content_version_id: "old-version", person_id: "ada", action: assigned }),
  ],
  people: [
    { person_id: "ada", display_name: "Ada", state: "curated" },
    { person_id: "grace", display_name: "Grace", state: "curated" },
    { person_id: "other", display_name: "Other", state: "curated" },
  ],
});
const openConflict = async (people: People) => {
  people.items = [ada, grace];
  const view = render(PeopleView, {
    people,
    cache: new PhotoPreviewCache("session", vi.fn()),
    onperson: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Ada" }));
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Grace" }));
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge people" });
  await fireEvent.click(within(dialog).getAllByRole("radio")[0]!);
  await fireEvent.click(within(dialog).getByRole("checkbox", { name: "Confirm merge" }));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Merge" }));
  return { view, dialog };
};
it.each(["assert", "withdrawn"] as const)(
  "withdraws the blocking rejection on the exact retained face (%s assignment), then asks again",
  async (assigned) => {
    const people = new People("session", vi.fn());
    const faces = vi.spyOn(api, "listPhotoFaces").mockResolvedValue(conflictFaces(assigned));
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
    const { view, dialog } = await openConflict(people);
    const withdraw = await within(dialog).findByRole("button", { name: 'Withdraw "not Grace"' });
    expect(faces).toHaveBeenCalledWith("old-version", expect.anything());
    // A rejection of someone outside this merge does not block it.
    expect(within(dialog).queryByRole("button", { name: 'Withdraw "not Other"' })).toBeNull();
    await fireEvent.click(withdraw);
    await fireEvent.click(
      within(
        screen.getByRole("dialog", {
          name: 'Withdraw "not Grace" from Face 1? The face keeps its current name.',
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
  },
);
it("keeps the rejection and the conflict when withdrawal is cancelled", async () => {
  const people = new People("session", vi.fn());
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue(conflictFaces("assert"));
  const decide = vi.spyOn(api, "decidePhotoFace");
  vi.spyOn(api, "mergePerson").mockRejectedValue(
    new APIError(
      "Review faces",
      409,
      "person_merge_conflict",
      undefined,
      [],
      [{ face_id: "conflict", version: "old-version" }],
    ),
  );
  const { view, dialog } = await openConflict(people);
  await fireEvent.click(
    await within(dialog).findByRole("button", { name: 'Withdraw "not Grace"' }),
  );
  await fireEvent.click(
    within(
      screen.getByRole("dialog", {
        name: 'Withdraw "not Grace" from Face 1? The face keeps its current name.',
      }),
    ).getByRole("button", { name: "Cancel" }),
  );
  expect(decide).not.toHaveBeenCalled();
  expect(within(dialog).getByRole("list", { name: "Faces to review" })).toBeTruthy();
  view.unmount();
  people.dispose();
});
it("clears the selection, Hidden counts, and an open merge when Hidden locks", async () => {
  const access = unlockedAccess();
  const people = new People("session", vi.fn(), access);
  people.items = [ada, grace];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Ada" }));
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Grace" }));
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge people" });
  expect(within(dialog).getByText("1 Hidden photo")).toBeTruthy();
  await act(() => access.acceptState({ configured: true }, Date.now() + 1));
  // The App conceals the cards when the scope changes.
  await act(() => people.conceal());
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(screen.queryByRole("region", { name: "Selected people" })).toBeNull();
  expect(screen.queryByText("1 Hidden photo")).toBeNull();
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
    { ...same, person_id: "alex-3", identity_label: "alex@example.test" },
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
  expect(within(dialog).getByRole("radio", { name: "Keep Alex · alex@example.test" })).toBeTruthy();
  await act(() => {
    people.merge = { ...people.merge!, notice: "stale" };
  });
  expect(within(dialog).getByRole("status").textContent).toContain(
    `Alex · ${detail} or Alex · alex@example.test changed.`,
  );
  view.unmount();
  people.dispose();
  await cache.dispose();
});
