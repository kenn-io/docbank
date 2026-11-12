import { afterEach, expect, it, vi } from "vite-plus/test";
import { groupByFiler, TaxesView, taxPageSize } from "./taxes.svelte.js";
import { holdings, party, record, serveTaxes, taxQuery, ventures } from "./tax-test-fixtures.js";

afterEach(() => vi.restoreAllMocks());

const page = (filer: string, count: number, from = 0) =>
  Array.from({ length: count }, (_, index) => record(`r${from + index}`, filer));

function view() {
  const onauthfailure = vi.fn();
  return { taxes: new TaxesView("synthetic", onauthfailure), onauthfailure };
}

const recordQueries = (fetch: ReturnType<typeof serveTaxes>) =>
  fetch.mock.calls
    .map(([input]) => new URL(String(input), location.origin))
    .filter((url) => url.pathname === "/api/v1/tax/records")
    .map(taxQuery);

it("first page requests limit 50 offset 0 with no filters", async () => {
  const fetch = serveTaxes(() => ({ items: [] }));
  const { taxes } = view();
  await taxes.load();
  expect(recordQueries(fetch)).toEqual([{ limit: "50", offset: "0" }]);
  expect(taxes.nextOffset).toBeNull();
  taxes.dispose();
});

it("filer and year filters are sent exactly and reset paging", async () => {
  const fetch = serveTaxes((query) => ({
    items: query.filer ? [record("v1", ventures)] : [record("h1", holdings)],
  }));
  const { taxes } = view();
  await taxes.load();
  await taxes.setFilters({ filer: ventures, year: 2031 });
  expect(recordQueries(fetch).at(-1)).toEqual({
    limit: "50",
    offset: "0",
    filer: ventures,
    year: "2031",
  });
  expect(taxes.rows.map((row) => row.record_id)).toEqual(["v1"]);
  taxes.dispose();
});

it("load more keeps filters and uses page offset plus items", async () => {
  const fetch = serveTaxes((query) =>
    query.offset === "0"
      ? { items: page(holdings, taxPageSize), offset: 0 }
      : { items: page(holdings, 3, 50), offset: 50 },
  );
  const { taxes } = view();
  await taxes.setFilters({ filer: holdings, year: 2031 });
  expect(taxes.nextOffset).toBe(50);
  await taxes.loadMore();
  expect(recordQueries(fetch).at(-1)).toEqual({
    limit: "50",
    offset: "50",
    filer: holdings,
    year: "2031",
  });
  expect(taxes.rows).toHaveLength(53);
  expect(taxes.nextOffset).toBeNull();
  taxes.dispose();
});

it("a response for an earlier filter is dropped", async () => {
  let answer!: (body: object) => void;
  const fetch = serveTaxes((query) =>
    query.filer === ventures
      ? { items: [record("v1", ventures)] }
      : new Promise<object>((resolve) => (answer = resolve)),
  );
  const { taxes } = view();
  const first = taxes.load();
  await vi.waitFor(() => expect(answer).toBeTypeOf("function"));
  await taxes.setFilters({ filer: ventures, year: null });
  answer({ items: [record("h1", holdings)] });
  await first;
  expect(taxes.rows.map((row) => row.record_id)).toEqual(["v1"]);
  const firstRecords = fetch.mock.calls.find(([input]) => String(input).includes("/tax/records"));
  expect(firstRecords?.[1]?.signal?.aborted).toBe(true);
  taxes.dispose();
});

it("clear resets both filters and reloads", async () => {
  const fetch = serveTaxes(() => ({ items: [] }));
  const { taxes } = view();
  await taxes.setFilters({ filer: holdings, year: 2031 });
  await taxes.clear();
  expect(taxes.filters).toEqual({ filer: "", year: null });
  expect(recordQueries(fetch).at(-1)).toEqual({ limit: "50", offset: "0" });
  taxes.dispose();
});

it("groups rows by filer in first-appearance order and keeps server order", async () => {
  const [h1, v1, h2] = [record("h1", holdings), record("v1", ventures), record("h2", holdings)];
  expect(groupByFiler([h1, v1, h2])).toStrictEqual([
    { filerPartyID: holdings, rows: [h1, h2] },
    { filerPartyID: ventures, rows: [v1] },
  ]);
  const first = [h1, v1, h2, ...page(ventures, taxPageSize - 3, 100)];
  serveTaxes((query) => ({
    items: query.offset === "0" ? first : [record("v2", ventures), record("h3", holdings)],
  }));
  const { taxes } = view();
  await taxes.load();
  await taxes.loadMore();
  const ids = taxes.groups.map((group) => [group.filerPartyID, group.rows.map((r) => r.record_id)]);
  expect(ids[0]).toEqual([holdings, ["h1", "h2", "h3"]]);
  expect(ids[1]![0]).toBe(ventures);
  expect((ids[1]![1] as string[]).at(-1)).toBe("v2");
  taxes.dispose();
});

it("parties page until a short page and map names", async () => {
  const many = Array.from({ length: 1000 }, (_, index) =>
    party(`p${index}`, `Example Party ${index}`, false),
  );
  const fetch = serveTaxes(
    () => ({ items: [] }),
    (query) =>
      query.offset === "0"
        ? { items: many, limit: 1000, offset: 0 }
        : {
            items: [
              party(ventures, "Example Ventures Ltd.", true, "2031-01-01T00:00:00Z"),
              party(holdings, "Example Holdings Ltd."),
            ],
            limit: 1000,
            offset: 1000,
          },
  );
  const { taxes } = view();
  await taxes.load();
  const partyQueries = fetch.mock.calls
    .map(([input]) => new URL(String(input), location.origin))
    .filter((url) => url.pathname === "/api/v1/parties")
    .map(taxQuery);
  expect(partyQueries).toEqual([
    { include_retired: "true", limit: "1000", offset: "0" },
    { include_retired: "true", limit: "1000", offset: "1000" },
  ]);
  expect(taxes.parties.get(holdings)?.display_name).toBe("Example Holdings Ltd.");
  expect(taxes.filers.map((filer) => filer.party_id)).toEqual([holdings, ventures]);
  taxes.dispose();
});

it("party failure keeps records readable", async () => {
  serveTaxes(
    () => ({ items: [record("h1", holdings)] }),
    () => new Response("{}", { status: 500 }),
  );
  const { taxes } = view();
  await taxes.load();
  expect(taxes.partyError).toBe("Filers could not be loaded.");
  expect(taxes.rows.map((row) => row.record_id)).toEqual(["h1"]);
  taxes.dispose();
});

it("errors are fixed text and 401 signs out", async () => {
  serveTaxes(
    () => Response.json({ detail: "SENSITIVE-ZZ-123" }, { status: 500 }),
    () => Response.json({ detail: "SENSITIVE-ZZ-123" }, { status: 500 }),
  );
  const first = view();
  await first.taxes.load();
  expect(first.taxes.error).toBe("Tax records could not be loaded.");
  expect(JSON.stringify([first.taxes.error, first.taxes.partyError])).not.toContain("SENSITIVE");
  first.taxes.dispose();

  vi.restoreAllMocks();
  const expired = () => Response.json({ detail: "Session expired" }, { status: 401 });
  serveTaxes(expired, expired);
  const second = view();
  await second.taxes.load();
  expect(second.onauthfailure).toHaveBeenCalledTimes(1);
  second.taxes.dispose();
});

it("retry repeats the failed load-more request", async () => {
  let fail = true;
  const fetch = serveTaxes((query) => {
    if (query.offset === "0") return { items: page(holdings, taxPageSize) };
    if (fail) return new Response("{}", { status: 503 });
    return { items: [record("h-last", holdings)], offset: 50 };
  });
  const { taxes } = view();
  await taxes.load();
  await taxes.loadMore();
  expect(taxes.error).toBe("Tax records could not be loaded.");
  expect(taxes.rows).toHaveLength(50);
  fail = false;
  await taxes.retry();
  expect(recordQueries(fetch).at(-1)?.offset).toBe("50");
  expect(taxes.rows).toHaveLength(51);
  expect(taxes.error).toBe("");
  taxes.dispose();
});

it("never asks for accounts, reveal or writes", async () => {
  const fetch = serveTaxes((query) =>
    query.offset === "0" ? { items: page(holdings, taxPageSize) } : { items: [], offset: 50 },
  );
  const { taxes } = view();
  await taxes.load();
  await taxes.setFilters({ filer: holdings, year: 2031 });
  await taxes.loadMore();
  await taxes.clear();
  expect(fetch).toHaveBeenCalled();
  for (const [input, init] of fetch.mock.calls) {
    const url = new URL(String(input), location.origin);
    expect(init?.method ?? "GET").toBe("GET");
    expect(["/api/v1/tax/records", "/api/v1/parties"]).toContain(url.pathname);
    expect(taxQuery(url)).not.toHaveProperty("reveal");
  }
  taxes.dispose();
});
