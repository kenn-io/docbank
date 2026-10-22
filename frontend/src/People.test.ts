import { afterEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PeopleView from "./People.svelte";
import { People } from "./people.svelte.js";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { ada, grace, personMergeReceipt, personResponse } from "./people-test-fixtures.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
const unlockedAccess = () => {
  const access = new HiddenAccess("session", vi.fn());
  access.acceptState({ configured: true, expires_in: 60, expires_at: "later" }, Date.now());
  return access;
};
it("keeps the search text with the applied query when the view returns", async () => {
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada] });
  const people = new People("session", vi.fn());
  const cache = new PhotoPreviewCache("session", vi.fn());
  let view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.input(screen.getByRole("textbox", { name: "Find people" }), {
    target: { value: "Ad" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await screen.findByRole("button", { name: "Ada 3 Library photos" });
  view.unmount();
  people.conceal();
  view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await people.load();
  expect(read.mock.calls.at(-1)![0]).toMatchObject({ query: "Ad" });
  expect((screen.getByRole("textbox", { name: "Find people" }) as HTMLInputElement).value).toBe(
    "Ad",
  );
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("renames a person with Library photos", async () => {
  const people = new People("session", vi.fn());
  people.items = [grace];
  const rename = vi
    .spyOn(api, "renamePerson")
    .mockImplementation(async (_id, _request, _headers) =>
      personResponse(grace, { display_name: "Grace Hopper", revision: 5 }),
    );
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
  expect(screen.getByRole("button", { name: "Grace Hopper 2 Library photos" })).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it.each([false, true])("submits selected cards and reports a locked merge=%s", async (locked) => {
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [grace] });
  const merge = vi
    .spyOn(api, "mergePerson")
    .mockImplementation(async (personID, request, _headers) => {
      if (locked) throw new APIError("unlock Hidden and retry the merge", 403, "hidden_locked");
      const source = [ada, grace].find(
        (person) => person.person_id === request.absorbed_person_id,
      )!;
      const survivor = [ada, grace].find((person) => person.person_id === personID)!;
      return personMergeReceipt(source, survivor, request.operation_id);
    });
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(
    screen.getByRole("checkbox", {
      name: `Select Ada · ${new Date(ada.created_at).toLocaleDateString()}`,
    }),
  );
  expect(screen.getByRole("button", { name: "Merge" }).hasAttribute("disabled")).toBe(true);
  await fireEvent.click(
    screen.getByRole("checkbox", {
      name: `Select Grace · ${new Date(grace.created_at).toLocaleDateString()}`,
    }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge people" });
  // Both cards show their Library evidence side by side.
  expect(within(dialog).getByText("3 Library photos")).toBeTruthy();
  expect(within(dialog).getByText("2 Library photos")).toBeTruthy();
  expect(within(dialog).queryByRole("checkbox", { name: "Confirm merge" })).toBeNull();
  const detail = new Date(grace.created_at).toLocaleDateString();
  await fireEvent.click(within(dialog).getByRole("radio", { name: `Keep Grace · ${detail}` }));
  const confirm = within(dialog).getByRole("button", { name: "Merge" });
  expect(confirm.hasAttribute("disabled")).toBe(true);
  await fireEvent.click(within(dialog).getByRole("checkbox", { name: "Confirm merge" }));
  await fireEvent.click(confirm);
  await waitFor(() => expect(merge).toHaveBeenCalledOnce());
  expect(merge.mock.calls[0]![0]).toBe("grace");
  if (locked) expect(await screen.findByText("unlock Hidden and retry the merge")).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("clears the selected cards and open merge when People is concealed", async () => {
  const access = unlockedAccess();
  const people = new People("session", vi.fn(), access);
  people.items = [ada, grace];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(
    screen.getByRole("checkbox", {
      name: `Select Ada · ${new Date(ada.created_at).toLocaleDateString()}`,
    }),
  );
  await fireEvent.click(
    screen.getByRole("checkbox", {
      name: `Select Grace · ${new Date(grace.created_at).toLocaleDateString()}`,
    }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge people" });
  expect(within(dialog).getByText("3 Library photos")).toBeTruthy();
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
  const checkboxes = [
    screen.getByRole("checkbox", { name: `Select Alex · ${detail} · alex-1` }),
    screen.getByRole("checkbox", { name: `Select Alex · ${detail} · alex-2` }),
    screen.getByRole("checkbox", { name: "Select Alex · alex@example.org" }),
  ];
  await fireEvent.click(checkboxes[0]!);
  await fireEvent.click(checkboxes[1]!);
  expect(
    within(screen.getByRole("region", { name: "Selected people" })).getByText(
      `Alex · ${detail} · alex-1 and Alex · ${detail} · alex-2 selected`,
    ),
  ).toBeTruthy();
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
  await fireEvent.click(checkboxes[2]!);
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  dialog = screen.getByRole("dialog", { name: "Merge people" });
  expect(within(dialog).getByRole("radio", { name: `Keep Alex · ${detail}` })).toBeTruthy();
  expect(within(dialog).getByRole("radio", { name: "Keep Alex · alex@example.org" })).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});

it("opens unlocked Hidden counts in Hidden and conceals them on lock", async () => {
  const access = unlockedAccess();
  const people = new People("session", vi.fn(), access);
  people.items = [{ ...grace, photo_summary: { library_count: 0, hidden_count: 1 } }];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const open = vi.fn();
  const view = render(PeopleView, { people, cache, onperson: open });
  await fireEvent.click(screen.getByRole("button", { name: "1 Hidden photo" }));
  expect(open).toHaveBeenLastCalledWith("grace", "Grace", true);
  await fireEvent.click(screen.getByRole("button", { name: "Grace 0 Library photos" }));
  expect(open).toHaveBeenLastCalledWith("grace", "Grace", true);
  await act(() => access.acceptState({ configured: true }, Date.now() + 1));
  expect(screen.queryByRole("button", { name: "1 Hidden photo" })).toBeNull();
  view.unmount();
  people.dispose();
  access.dispose();
  await cache.dispose();
});

it("distinguishes cards with repeated names using their identity details", async () => {
  const people = new People("session", vi.fn());
  people.items = [
    { ...ada, display_name: "Ada", identity_label: "ada@example.com" },
    { ...grace, display_name: "Ada", identity_label: "other@example.com" },
  ];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  expect(
    screen.getByRole("button", { name: "Ada · ada@example.com 3 Library photos" }),
  ).toBeTruthy();
  expect(
    screen.getByRole("button", { name: "Ada · other@example.com 2 Library photos" }),
  ).toBeTruthy();
  await act(() => (people.items = [{ ...ada, identity_label: "ada@example.com" }]));
  expect(screen.getByRole("button", { name: "Ada 3 Library photos" })).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});
