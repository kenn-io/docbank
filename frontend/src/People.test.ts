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
it.each([false, true])("submits the dialog's rename revision after refresh=%s", async (refresh) => {
  const current = { ...grace, display_name: "Grace Murray", revision: 5 };
  vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [refresh ? current : { ...grace, display_name: "Grace Hopper", revision: 5 }],
  });
  const access = new HiddenAccess("session", vi.fn());
  const people = new People("session", vi.fn(), access);
  people.items = [grace];
  const rename = vi
    .spyOn(api, "renamePerson")
    .mockImplementation(async (_id, _request, headers) => {
      if (refresh && headers?.["If-Match"] !== '"5"')
        throw new APIError("Person changed; review the current name", 412, "stale_revision");
      return personResponse(grace, { display_name: "Grace Hopper", revision: 5 });
    });
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Rename" }));
  const dialog = screen.getByRole("dialog", { name: "Rename Grace" });
  if (refresh) await act(() => people.refresh());
  await fireEvent.input(within(dialog).getByRole("textbox", { name: "Person name" }), {
    target: { value: "Grace Hopper" },
  });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
  await waitFor(() => expect(rename).toHaveBeenCalledOnce());
  if (refresh) {
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "Person changed; review the current name",
    );
    expect(
      await screen.findByRole("button", { name: "Grace Murray 2 Library photos" }),
    ).toBeTruthy();
  } else {
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByRole("button", { name: "Grace Hopper 2 Library photos" })).toBeTruthy();
  }
  expect(rename).toHaveBeenCalledOnce();
  expect(rename.mock.calls[0]!.slice(0, 3)).toEqual([
    grace.person_id,
    { display_name: "Grace Hopper" },
    { "If-Match": `"${grace.revision}"` },
  ]);
  view.unmount();
  people.dispose();
  access.dispose();
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
  expect(merge.mock.calls[0]![0]).toBe(grace.person_id);
  expect(merge.mock.calls[0]![1].absorbed_person_id).toBe(ada.person_id);
  if (locked) expect(await screen.findByText("unlock Hidden and retry the merge")).toBeTruthy();
  else await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  view.unmount();
  people.dispose();
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
  let nextURL = 0;
  const revokeObjectURL = vi.fn();
  vi.stubGlobal(
    "URL",
    class extends URL {
      static createObjectURL = vi.fn(() => `blob:person-${++nextURL}`);
      static revokeObjectURL = revokeObjectURL;
    },
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
          generation_id: "",
          rotation: 0,
          box: { x: 0.1, y: 0.1, width: 0.2, height: 0.2 },
        },
      },
    },
  ];
  vi.spyOn(api, "preparePhotoPreview").mockResolvedValue({
    state: "ready",
    generation_id: "ready",
  });
  const cache = new PhotoPreviewCache("session", vi.fn());
  const get = vi.spyOn(cache, "get").mockResolvedValue(new Blob(["synthetic"]));
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await waitFor(() => expect(screen.getByRole("img", { name: "Face of Ada" })).toBeTruthy());
  expect(get.mock.calls[0]![4]).toBe(ada.person_id);
  // An in-place card update with the same face keeps its portrait.
  await act(() => (people.items = [{ ...people.items[0]!, display_name: "Ada L", revision: 3 }]));
  expect(screen.getByRole("img", { name: "Face of Ada L" })).toBeTruthy();
  expect(get).toHaveBeenCalledOnce();
  await act(() => people.toggle(people.items[0]!));
  const current = people.items[0]!;
  vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [
      {
        ...current,
        photo_summary: {
          ...current.photo_summary!,
          face: { ...current.photo_summary!.face!, generation_id: "ready" },
        },
      },
    ],
  });
  await act(() => people.load("Ada"));
  await waitFor(() => expect(screen.getByRole("img", { name: "Face of Ada L" })).toBeTruthy());
  await waitFor(() => expect(get).toHaveBeenCalledTimes(2));
  const portraitURL = screen.getByRole("img", { name: "Face of Ada L" }).getAttribute("src");
  revokeObjectURL.mockClear();
  await act(() => people.conceal());
  expect(revokeObjectURL).toHaveBeenCalledWith(portraitURL);
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("labels namesakes by detail, and by ID only when the details match", async () => {
  const alex1 = "00000000-0000-4000-8000-000000000011";
  const alex2 = "00000000-0000-4000-8000-000000000012";
  const alex3 = "00000000-0000-4000-8000-000000000013";
  const same = { ...grace, display_name: "Alex", display_name_folded: "alex" };
  const people = new People("session", vi.fn());
  people.items = [
    { ...same, person_id: alex1 },
    { ...same, person_id: alex2 },
    { ...same, person_id: alex3, identity_label: "alex@example.org" },
  ];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  const detail = new Date(same.created_at).toLocaleDateString();
  const checkboxes = [
    screen.getByRole("checkbox", { name: `Select Alex · ${detail} · ${alex1}` }),
    screen.getByRole("checkbox", { name: `Select Alex · ${detail} · ${alex2}` }),
    screen.getByRole("checkbox", { name: "Select Alex · alex@example.org" }),
  ];
  await fireEvent.click(checkboxes[0]!);
  await fireEvent.click(checkboxes[1]!);
  expect(
    within(screen.getByRole("region", { name: "Selected people" })).getByText(
      `Alex · ${detail} · ${alex1} and Alex · ${detail} · ${alex2} selected`,
    ),
  ).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  let dialog = screen.getByRole("dialog", { name: "Merge people" });
  await fireEvent.click(
    within(dialog).getByRole("radio", { name: `Keep Alex · ${detail} · ${alex2}` }),
  );
  expect(
    within(dialog).getByText(
      `Keep Alex · ${detail} · ${alex2}. Move Alex · ${detail} · ${alex1}'s records into this person.`,
    ),
  ).toBeTruthy();
  await fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await fireEvent.click(checkboxes[1]!);
  await fireEvent.click(checkboxes[2]!);
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  dialog = screen.getByRole("dialog", { name: "Merge people" });
  expect(within(dialog).getByRole("radio", { name: `Keep Alex · ${detail}` })).toBeTruthy();
  expect(within(dialog).getByRole("radio", { name: "Keep Alex · alex@example.org" })).toBeTruthy();
  await fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await act(
    () => (people.items = [{ ...same, person_id: alex3, identity_label: "alex@example.org" }]),
  );
  expect(screen.getByRole("button", { name: "Alex 2 Library photos" })).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});

it("opens unlocked Hidden counts in Hidden and conceals them on lock", async () => {
  const access = unlockedAccess();
  const people = new People("session", vi.fn(), access);
  people.items = [
    { ...ada, photo_summary: { library_count: 3, hidden_count: 2 } },
    { ...grace, photo_summary: { library_count: 0, hidden_count: 1 } },
    {
      ...grace,
      person_id: "00000000-0000-4000-8000-000000000003",
      display_name: "Lin",
      photo_summary: { library_count: 1, hidden_count: 0 },
    },
  ];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const open = vi.fn();
  const view = render(PeopleView, { people, cache, onperson: open });
  expect(screen.queryByRole("button", { name: "0 Hidden photos" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "2 Hidden photos" }));
  expect(open).toHaveBeenLastCalledWith(ada.person_id, "Ada", true);
  await fireEvent.click(screen.getByRole("button", { name: "1 Hidden photo" }));
  expect(open).toHaveBeenLastCalledWith(grace.person_id, "Grace", true);
  await fireEvent.click(screen.getByRole("button", { name: "Grace 0 Library photos" }));
  expect(open).toHaveBeenLastCalledWith(grace.person_id, "Grace", true);
  // An access-scope change closes the merge dialog.
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
  await act(() => access.acceptState({ configured: true }, Date.now() + 1));
  expect(screen.queryByRole("dialog")).toBeNull();
  // App conceals the old cards before loading the locked projection.
  await act(() => people.conceal());
  expect(screen.queryByRole("button", { name: "1 Hidden photo" })).toBeNull();
  expect(screen.queryByRole("button", { name: "2 Hidden photos" })).toBeNull();
  view.unmount();
  people.dispose();
  access.dispose();
  await cache.dispose();
});
