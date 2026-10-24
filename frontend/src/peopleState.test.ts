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
    .mockRejectedValueOnce(new APIError("People scope changed", 422, "people_cursor_expired"))
    .mockResolvedValueOnce({ items: [grace] });
  const people = new People("session", vi.fn());
  await people.load();
  await people.load(people.query, true);
  expect(people.error).toBe("People scope changed");
  await people.retry();
  expect(people.items).toEqual([grace]);
  expect(read.mock.calls[2]![0]).not.toHaveProperty("cursor");
  people.dispose();
});

it.each(["valid", "empty", "wrong person", "invalid revision", "wrong ETag"])(
  "renames with a %s response",
  async (result) => {
    const read = vi.spyOn(api, "listPeople");
    const saved = await personResponse(ada, { display_name: "Zoe", revision: 3 }).json();
    const body =
      result === "empty"
        ? {}
        : {
            ...saved,
            ...(result === "wrong person" ? { person_id: "someone-else" } : {}),
            ...(result === "invalid revision" ? { revision: 0 } : {}),
          };
    const rename = vi.spyOn(api, "renamePerson").mockResolvedValue(
      Response.json(body, {
        headers: {
          ETag: result === "wrong ETag" ? '"4"' : result === "invalid revision" ? '"0"' : '"3"',
        },
      }),
    );
    const changed = vi.fn();
    const people = new People("session", vi.fn(), undefined, changed);
    people.items = [ada];
    people.query = "Ada";
    people.cursor = "old-page";
    people.toggle(ada);
    read.mockResolvedValue({ items: result === "valid" ? [] : [ada] });
    expect(await people.rename(ada, " Zoe ")).toBe(result === "valid");
    expect(rename.mock.calls[0]!.slice(0, 3)).toEqual([
      "ada",
      { display_name: "Zoe" },
      { "If-Match": '"2"' },
    ]);
    if (result !== "valid") {
      expect(people.writeError).toBe("Invalid person response.");
      expect(changed).not.toHaveBeenCalled();
      expect(people.items).toEqual([ada]);
      expect(people.selected).toEqual([]);
      expect(read.mock.calls.map(([params]) => params?.query)).toEqual(["Ada", "Zoe"]);
      people.dispose();
      return;
    }
    expect(people.items).toEqual([]);
    expect(people.cursor).toBeUndefined();
    expect(people.selected[0]).toMatchObject({
      person_id: "ada",
      display_name: "Zoe",
      revision: 3,
    });
    expect(changed).toHaveBeenCalledWith("ada", "Zoe");
    expect(read).toHaveBeenCalledOnce();
    expect(read.mock.calls[0]![0]).toMatchObject({ query: "Ada" });
    expect(read.mock.calls[0]![0]).not.toHaveProperty("cursor");
    people.dispose();
  },
);

it.each([
  "valid",
  "empty",
  "wrong survivor",
  "wrong source",
  "wrong operation",
  "missing merge ID",
  "wrong ETag",
])("reloads after merging with a %s receipt", async (result) => {
  const kept = { ...grace, revision: 5, photo_summary: { library_count: 3 } };
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [kept] });
  const merge = vi.spyOn(api, "mergePerson").mockImplementation(async (_, request) => {
    const receipt = await personMergeReceipt(ada, grace, request.operation_id).json();
    const body =
      result === "empty"
        ? {}
        : {
            ...receipt,
            ...(result === "wrong survivor" ? { survivor_person_id: "someone-else" } : {}),
            ...(result === "wrong source" ? { absorbed_person_id: "someone-else" } : {}),
            ...(result === "wrong operation"
              ? { operation_id: "00000000-0000-4000-8000-000000000098" }
              : {}),
            ...(result === "missing merge ID" ? { merge_id: "" } : {}),
          };
    return Response.json(body, { headers: { ETag: result === "wrong ETag" ? '"4"' : '"5"' } });
  });
  const changed = vi.fn();
  const people = new People("session", vi.fn(), undefined, changed);
  people.items = [ada, grace];
  people.cursor = "old-page";
  merging(people);
  expect(await people.submitMerge()).toBe(result === "valid");
  expect(merge.mock.calls[0]!.slice(0, 3)).toEqual([
    "grace",
    { absorbed_person_id: "ada", absorbed_revision: 2, operation_id: expect.any(String) },
    { "If-Match": '"4"' },
  ]);
  expect(people.items).toEqual([kept]);
  expect(people.merge).toBeUndefined();
  expect(people.selected).toEqual([]);
  expect(read.mock.calls[0]![0]).not.toHaveProperty("cursor");
  if (result === "valid") expect(changed).toHaveBeenCalledExactlyOnceWith("ada", "Grace", "grace");
  else {
    expect(changed).not.toHaveBeenCalled();
    expect(people.writeError).toBe("Invalid person merge response.");
  }
  people.dispose();
});

it.each([
  [409, "person_merge_conflict"],
  [409, "person_retired"],
  [412, "stale_revision"],
] as const)("shows the server detail and reloads after merge HTTP %s %s", async (status, code) => {
  const detail = "Review face face-a on version version-a; more faces also conflict";
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [grace] });
  vi.spyOn(api, "mergePerson").mockRejectedValue(new APIError(detail, status, code));
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  people.cursor = "old-page";
  merging(people);
  expect(await people.submitMerge()).toBe(false);
  expect(people.writeError).toBe(detail);
  expect(people.items).toEqual([grace]);
  expect(people.selected).toEqual(code === "person_retired" || status === 412 ? [] : [ada, grace]);
  expect(people.merge).toBeUndefined();
  expect(read.mock.calls[0]![0]).not.toHaveProperty("cursor");
  people.dispose();
});

it("recovers after rename HTTP 422", async () => {
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [grace] });
  const detail = "Name is too long";
  vi.spyOn(api, "renamePerson").mockRejectedValue(new APIError(detail, 422, "validation"));
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  people.cursor = "old-page";
  people.toggle(ada);
  people.toggle(grace);
  expect(await people.rename(ada, "x".repeat(1000))).toBe(false);
  expect(people.writeError).toBe(detail);
  expect(people.selected).toEqual([ada, grace]);
  expect(people.items).toEqual([ada, grace]);
  expect(people.cursor).toBe("old-page");
  expect(read).not.toHaveBeenCalled();
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
  else reject(new APIError("Invalid input", 422, "validation"));
  await pending;
  expect(people.items).toEqual([]);
  expect(people.selected).toEqual([]);
  expect(people.merge).toBeUndefined();
  expect(people.writeError).toBe("");
  expect(people.busy).toBe(false);
  if (action === "load") {
    expect(read).toHaveBeenCalledOnce();
    expect(read.mock.calls[0]![1]!.signal!.aborted).toBe(true);
  } else expect(read).not.toHaveBeenCalled();
  people.dispose();
});

it.each([true, false])(
  "confirms a lost rename response after leaving People, committed=%s",
  async (committed) => {
    let reject!: (cause: Error) => void;
    let rows = [ada];
    const read = vi.spyOn(api, "listPeople").mockImplementation(async () => ({ items: rows }));
    vi.spyOn(api, "renamePerson").mockImplementation(
      () =>
        new Promise((_, fail) => {
          if (committed) rows = [{ ...ada, display_name: "Zoe", revision: 3 }];
          else rows = [];
          reject = fail;
        }),
    );
    const changed = vi.fn();
    const people = new People("session", vi.fn(), undefined, changed);
    people.items = [ada, grace];
    merging(people);
    const pending = people.rename(ada, " Zoe ");
    people.conceal();
    reject(new TypeError("Response lost"));
    expect(await pending).toBe(false);
    if (committed) expect(changed).toHaveBeenCalledExactlyOnceWith("ada", "Zoe");
    else expect(changed).not.toHaveBeenCalled();
    expect(read).toHaveBeenCalledExactlyOnceWith(
      { query: "Zoe", limit: peoplePageSize, photo_summary: true },
      { session: "session", signal: expect.any(AbortSignal) },
    );
    expect(people.items).toEqual([]);
    expect(people.selected).toEqual([]);
    expect(people.merge).toBeUndefined();
    expect(people.writeError).toBe("");
    people.dispose();
  },
);

it("confirms a lost merge response after leaving People without restoring the directory", async () => {
  let reject!: (cause: Error) => void;
  let rows = [ada, grace];
  const read = vi.spyOn(api, "listPeople").mockImplementation(async () => ({ items: rows }));
  const merge = vi
    .spyOn(api, "mergePerson")
    .mockImplementationOnce(
      () =>
        new Promise((_, fail) => {
          rows = [grace];
          reject = fail;
        }),
    )
    .mockImplementation(async (_, request) => personMergeReceipt(ada, grace, request.operation_id));
  const changed = vi.fn();
  const people = new People("session", vi.fn(), undefined, changed);
  people.items = rows;
  merging(people);
  const pending = people.submitMerge();
  people.conceal();
  reject(new TypeError("Response lost"));
  expect(await pending).toBe(true);
  expect(merge).toHaveBeenCalledTimes(2);
  expect(merge.mock.calls[1]).toEqual(merge.mock.calls[0]);
  expect(changed).toHaveBeenCalledExactlyOnceWith("ada", "Grace", "grace");
  expect(read).not.toHaveBeenCalled();
  expect(people.items).toEqual([]);
  expect(people.selected).toEqual([]);
  expect(people.merge).toBeUndefined();
  expect(people.writeError).toBe("");
  people.dispose();
});

it.each([0, 503])("replays an uncertain merge HTTP %s once", async (status) => {
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [grace] });
  const merge = vi
    .spyOn(api, "mergePerson")
    .mockRejectedValueOnce(new APIError("Response lost", status, ""))
    .mockImplementation(async (_, request) => personMergeReceipt(ada, grace, request.operation_id));
  const changed = vi.fn();
  const people = new People("session", vi.fn(), undefined, changed);
  merging(people);
  expect(await people.submitMerge()).toBe(true);
  expect(merge).toHaveBeenCalledTimes(2);
  expect(merge.mock.calls[1]).toEqual(merge.mock.calls[0]);
  expect(changed).toHaveBeenCalledExactlyOnceWith("ada", "Grace", "grace");
  expect(people.merge).toBeUndefined();
  expect(people.selected).toEqual([]);
  people.dispose();
});

it.each([404, 412, 503, "invalid receipt"])(
  "leaves a merge unconfirmed after replay %s",
  async (result) => {
    const renamed = { ...ada, display_name: "Zoe", revision: 3 };
    vi.spyOn(api, "listPeople").mockImplementation(async (params) => ({
      items: [renamed, grace].filter((row) => row.display_name.includes(params?.query ?? "")),
    }));
    const merge = vi
      .spyOn(api, "mergePerson")
      .mockRejectedValueOnce(new APIError("Response lost", 0, ""))
      .mockImplementation(async () => {
        if (typeof result === "number") throw new APIError("Unconfirmed", result, "");
        return Response.json({});
      });
    const changed = vi.fn();
    const people = new People("session", vi.fn(), undefined, changed);
    merging(people);
    expect(await people.submitMerge()).toBe(false);
    expect(changed).not.toHaveBeenCalled();
    expect(merge).toHaveBeenCalledTimes(2);
    expect(merge.mock.calls[1]).toEqual(merge.mock.calls[0]);
    people.dispose();
  },
);
