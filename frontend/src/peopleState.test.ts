import { afterEach, expect, it, vi } from "vite-plus/test";
import { People, peoplePageSize } from "./people.svelte.js";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { ada, grace, person, personMergeReceipt, personResponse } from "./people-test-fixtures.js";

afterEach(() => vi.restoreAllMocks());
const merging = (people: People) => {
  people.toggle(ada);
  people.toggle(grace);
  people.startMerge();
  people.keep(grace.person_id);
  people.confirmMerge(true);
};

it("pages the projection", async () => {
  const read = vi
    .spyOn(api, "listPeople")
    .mockResolvedValueOnce({ items: [ada], next_cursor: "next" })
    .mockResolvedValueOnce({ items: [grace] });
  const people = new People("session", vi.fn());
  await people.load();
  await people.load("", true);
  expect(people.items.map((row) => row.person_id)).toEqual(["ada", "grace"]);
  expect(read.mock.calls[1]![0]).toEqual({
    query: "",
    limit: peoplePageSize,
    photo_summary: true,
    cursor: "next",
  });
  people.dispose();
});

it("retries from the first page when a saved People cursor expires", async () => {
  const read = vi
    .spyOn(api, "listPeople")
    .mockResolvedValueOnce({ items: [ada], next_cursor: "stale" })
    .mockRejectedValueOnce(new APIError("invalid people cursor", 422, "validation"))
    .mockResolvedValueOnce({ items: [grace] });
  const people = new People("session", vi.fn());
  await people.load();
  await people.load(people.query, true);
  expect(people.error).toBe("invalid people cursor");
  await people.retry();
  expect(people.items).toEqual([grace]);
  expect(read.mock.calls[2]![0]).not.toHaveProperty("cursor");
  people.dispose();
});

it("renames a loaded card and its selection from the response", async () => {
  const read = vi.spyOn(api, "listPeople");
  const rename = vi
    .spyOn(api, "renamePerson")
    .mockResolvedValue(personResponse(ada, { display_name: "Ada L", revision: 3 }));
  const changed = vi.fn();
  const people = new People("session", vi.fn(), undefined, changed);
  people.items = [ada, grace];
  people.toggle(ada);
  expect(await people.rename(ada, " Ada L ")).toBe(true);
  expect(rename.mock.calls[0]!.slice(0, 3)).toEqual([
    "ada",
    { display_name: "Ada L" },
    { "If-Match": '"2"' },
  ]);
  expect(people.items[0]).toMatchObject({ display_name: "Ada L", revision: 3 });
  expect(people.selected[0]).toEqual(people.items[0]);
  expect(changed).toHaveBeenCalledWith("ada", "Ada L");
  expect(read).not.toHaveBeenCalled();
  people.dispose();
});

it("reloads the first page after merging and reports the survivor", async () => {
  const kept = { ...grace, revision: 5, photo_summary: { library_count: 3 } };
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [kept] });
  const merge = vi
    .spyOn(api, "mergePerson")
    .mockImplementation(async (_, request) => personMergeReceipt(ada, grace, request.operation_id));
  const changed = vi.fn();
  const people = new People("session", vi.fn(), undefined, changed);
  people.items = [ada, grace];
  people.cursor = "old-page";
  merging(people);
  expect(await people.submitMerge()).toBe(true);
  expect(merge.mock.calls[0]!.slice(0, 3)).toEqual([
    "grace",
    { absorbed_person_id: "ada", absorbed_revision: 2, operation_id: expect.any(String) },
    { "If-Match": '"4"' },
  ]);
  expect(people.items).toEqual([kept]);
  expect(people.merge).toBeUndefined();
  expect(people.selected).toEqual([]);
  expect(read.mock.calls[0]![0]).not.toHaveProperty("cursor");
  expect(changed).toHaveBeenCalledWith("ada", "Grace", "grace");
  people.dispose();
});

it.each([409, 412, 503])(
  "shows the server detail and reloads after merge HTTP %s",
  async (status) => {
    const detail = "Review face face-a on version version-a; more faces also conflict";
    const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [grace] });
    vi.spyOn(api, "mergePerson").mockRejectedValue(
      new APIError(detail, status, "person_merge_conflict"),
    );
    const people = new People("session", vi.fn());
    people.items = [ada, grace];
    people.cursor = "old-page";
    merging(people);
    expect(await people.submitMerge()).toBe(false);
    expect(people.writeError).toBe(detail);
    expect(people.items).toEqual([grace]);
    if (status === 409) expect(people.selected).toEqual([ada, grace]);
    expect(people.merge).toBeUndefined();
    expect(read.mock.calls[0]![0]).not.toHaveProperty("cursor");
    people.dispose();
  },
);

it.each([412, 503, 422])("recovers after rename HTTP %s", async (status) => {
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [grace] });
  const detail = status === 422 ? "Name is too long" : "Rename failed";
  vi.spyOn(api, "renamePerson").mockRejectedValue(
    new APIError(detail, status, status === 422 ? "validation" : "stale_revision"),
  );
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  people.cursor = "old-page";
  people.toggle(ada);
  people.toggle(grace);
  expect(await people.rename(ada, status === 422 ? "x".repeat(1000) : "Ada L")).toBe(false);
  expect(people.writeError).toBe(detail);
  if (status === 422) {
    expect(people.selected).toEqual([ada, grace]);
    expect(people.items).toEqual([ada, grace]);
    expect(people.cursor).toBe("old-page");
    expect(read).not.toHaveBeenCalled();
  } else {
    expect(people.items).toEqual([grace]);
    expect(read.mock.calls[0]![0]).not.toHaveProperty("cursor");
  }
  people.dispose();
});

it("keeps two selections across searches and starts a merge", async () => {
  vi.spyOn(api, "listPeople")
    .mockResolvedValueOnce({ items: [ada] })
    .mockResolvedValueOnce({ items: [grace, person("zoe")] });
  const people = new People("session", vi.fn());
  await people.load("Ada");
  people.toggle(people.items[0]!);
  await people.load("G");
  people.toggle(people.items[0]!);
  people.toggle(people.items[1]!);
  expect(people.selected).toEqual([ada, grace]);
  people.startMerge();
  expect(people.merge?.pair).toEqual([ada, grace]);
  expect(people.merge?.keep).toBeUndefined();
  people.dispose();
});

it("reports sign-in expiry", async () => {
  vi.spyOn(api, "listPeople").mockRejectedValue(new APIError("Sign in", 401, "unauthorized"));
  const failed = vi.fn();
  const people = new People("session", failed);
  await people.load();
  expect(failed).toHaveBeenCalled();
  expect(people.error).toBe("Sign in");
  people.dispose();
});

it("deduplicates renamed cards crossing a saved cursor", async () => {
  const renamed = { ...ada, display_name: "Zoe", revision: 3 };
  vi.spyOn(api, "listPeople")
    .mockResolvedValueOnce({ items: [ada], next_cursor: "next" })
    .mockResolvedValueOnce({ items: [grace, renamed] });
  vi.spyOn(api, "renamePerson").mockResolvedValue(personResponse(renamed));
  const people = new People("session", vi.fn());
  await people.load();
  await people.rename(ada, "Zoe");
  await people.load("", true);
  expect(people.items.map((row) => row.person_id)).toEqual(["ada", "grace"]);
  people.dispose();
});

it.each(["rename", "merge", "load"])("conceals names after a late %s response", async (action) => {
  let reject!: (cause: Error) => void;
  let finish!: (page: api.PersonPage) => void;
  const read = vi.spyOn(api, "listPeople");
  if (action === "load")
    read.mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
  else if (action === "merge")
    vi.spyOn(api, "mergePerson").mockImplementation(
      () =>
        new Promise((_, fail) => {
          reject = fail;
        }),
    );
  else
    vi.spyOn(api, "renamePerson").mockImplementation(
      () =>
        new Promise((_, fail) => {
          reject = fail;
        }),
    );
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  merging(people);
  const pending =
    action === "load"
      ? people.load("Grace")
      : action === "merge"
        ? people.submitMerge()
        : people.rename(ada, "Ada L");
  people.conceal();
  if (action === "load") finish({ items: [grace] });
  else reject(new Error("Offline"));
  await pending;
  expect(people.items).toEqual([]);
  expect(people.merge).toBeUndefined();
  expect(people.writeError).toBe("");
  expect(people.busy).toBe(false);
  if (action === "load") {
    expect(read).toHaveBeenCalledOnce();
    expect(read.mock.calls[0]![1]!.signal!.aborted).toBe(true);
  } else expect(read).not.toHaveBeenCalled();
  people.dispose();
});
