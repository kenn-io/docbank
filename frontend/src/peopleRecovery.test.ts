import { afterEach, expect, it, vi } from "vite-plus/test";
import { People } from "./people.svelte.js";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { ada, grace } from "./people-test-fixtures.js";

afterEach(() => vi.restoreAllMocks());

it("keeps the selection and loaded pages after a rejected rename", async () => {
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada] });
  vi.spyOn(api, "renamePerson").mockRejectedValue(
    new APIError("Name is too long", 422, "validation"),
  );
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  people.cursor = "page-two";
  people.toggle(ada);
  people.toggle(grace);
  expect(await people.rename(ada, "x".repeat(1000))).toBe(false);
  expect(people.writeError).toBe("Name is too long");
  expect(people.selected).toEqual([ada, grace]);
  expect(people.items).toEqual([ada, grace]);
  expect(people.cursor).toBe("page-two");
  expect(read).not.toHaveBeenCalled();
  people.dispose();
});

it("keeps the selection after a merge conflict", async () => {
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada, grace] });
  vi.spyOn(api, "mergePerson").mockRejectedValue(
    new APIError("Review the rejected face", 409, "person_merge_conflict"),
  );
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  people.toggle(ada);
  people.toggle(grace);
  people.startMerge();
  people.keep(grace.person_id);
  people.confirmMerge(true);
  expect(await people.submitMerge()).toBe(false);
  expect(people.writeError).toBe("Review the rejected face");
  expect(people.selected).toEqual([ada, grace]);
  people.dispose();
});
