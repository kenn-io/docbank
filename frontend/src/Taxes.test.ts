import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import Taxes from "./Taxes.svelte";
import { drawerPage } from "./drawerNavigation.js";
import { holdings, record, serveTaxes, taxQuery, ventures } from "./tax-test-fixtures.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const q1 = record("h1", holdings, {
  title: "Sales tax return, Q1 2031",
  form_code: "Form 100",
  due_date: "2031-04-30",
  direction: "payable",
  amount_minor: 123400,
});
const notice = record("v1", ventures, {
  kind: "notice",
  title: "Notice of assessment 2031",
  currency: "JPY",
  direction: "payable",
  amount_minor: 5000,
});
const q2 = record("h2", holdings, { title: "Sales tax return, Q2 2031", due_date: "2031-07-31" });

const show = () =>
  render(Taxes, {
    props: { session: "synthetic", onauthfailure: vi.fn() },
    context: new Map([[drawerPage, true]]),
  });
const titles = (table: HTMLElement) =>
  within(table)
    .getAllByRole("row")
    .slice(1)
    .map((row) => within(row).getAllByRole("cell")[0]!.firstElementChild?.textContent);
const lastRecordQuery = (fetch: ReturnType<typeof serveTaxes>) =>
  taxQuery(
    new URL(
      String(fetch.mock.calls.findLast(([input]) => String(input).includes("/tax/records"))![0]),
      location.origin,
    ),
  );

it("shows loading, then grouped rows with state, due and exact amount", async () => {
  let answer!: (body: object) => void;
  serveTaxes(() => new Promise((resolve) => (answer = resolve)));
  show();
  expect(await screen.findByText(/Loading…/)).toBeTruthy();
  answer({ items: [q1, notice, q2] });
  const first = await screen.findByRole("table", { name: "Tax records for Example Holdings Ltd." });
  const headings = screen.getAllByRole("heading", { level: 3 });
  expect(headings.map((heading) => heading.textContent)).toEqual([
    "Example Holdings Ltd.",
    "Example Ventures Ltd.",
  ]);
  expect(headings.some((heading) => /\d/.test(heading.textContent ?? ""))).toBe(false);
  expect(titles(first)).toEqual(["Sales tax return, Q1 2031", "Sales tax return, Q2 2031"]);
  const row = within(first).getAllByRole("row")[1]!;
  for (const text of ["Form 100", "2031", "2031-04-30", "Expected", "123400 USD", "Payable"])
    expect(within(row).getByText(text)).toBeTruthy();
  expect(within(first).getByRole("columnheader", { name: "Amount (minor units)" })).toBeTruthy();
  const second = screen.getByRole("table", { name: "Tax records for Example Ventures Ltd." });
  expect(within(second).getByText("5000 JPY")).toBeTruthy();
  expect(screen.queryByRole("columnheader", { name: "Filer" })).toBeNull();
});

it("filer rail filters, marks the current filer and drops headings", async () => {
  const fetch = serveTaxes((query) => ({
    items: query.filer === ventures ? [notice] : [q1, notice, q2],
  }));
  show();
  await screen.findByRole("table", { name: "Tax records for Example Ventures Ltd." });
  const rail = screen.getByRole("navigation", { name: "Tax filters" });
  const all = within(rail).getByRole("button", { name: "All filers" });
  expect(all.getAttribute("aria-current")).toBe("page");
  const filer = within(rail).getByRole("button", { name: "Example Ventures Ltd." });
  await fireEvent.click(filer);
  const table = await screen.findByRole("table", { name: "Tax records" });
  expect(titles(table)).toEqual(["Notice of assessment 2031"]);
  expect(filer.getAttribute("aria-current")).toBe("page");
  expect(all.getAttribute("aria-current")).toBeNull();
  expect(screen.queryByRole("heading", { level: 3 })).toBeNull();
  expect(lastRecordQuery(fetch)).toEqual({ limit: "50", offset: "0", filer: ventures });
});

it("unsafe amount shows the hint, never digits", async () => {
  serveTaxes(() => ({ items: [{ ...q1, amount_minor: 9007199254740992 }] }));
  show();
  const cell = (await screen.findByText("Too large to show")).closest("td")!;
  expect(cell.title).toBe("Use docbank tax record show to see the exact amount");
  expect(cell.closest("tr")!.textContent).not.toContain("9007");
});

it("year field applies valid years and rejects others without a request", async () => {
  const fetch = serveTaxes(() => ({ items: [q1] }));
  show();
  await screen.findByText("Sales tax return, Q1 2031");
  const year = screen.getByLabelText("Tax year");
  await fireEvent.input(year, { target: { value: "2031" } });
  await fireEvent.keyDown(year, { key: "Enter" });
  await waitFor(() =>
    expect(lastRecordQuery(fetch)).toEqual({ limit: "50", offset: "0", year: "2031" }),
  );
  for (const value of ["0", "10000", "20x1"]) {
    const requests = fetch.mock.calls.length;
    await fireEvent.input(year, { target: { value } });
    await fireEvent.keyDown(year, { key: "Enter" });
    expect(screen.getByText("Enter a year from 1 to 9999")).toBeTruthy();
    expect(fetch.mock.calls.length).toBe(requests);
  }
  await fireEvent.click(screen.getByRole("button", { name: "All years" }));
  await waitFor(() => expect(lastRecordQuery(fetch)).toEqual({ limit: "50", offset: "0" }));
  expect(screen.queryByText("Enter a year from 1 to 9999")).toBeNull();
});

it("empty vault and no-match states", async () => {
  const fetch = serveTaxes(() => ({ items: [] }));
  show();
  expect(await screen.findByText("No tax records yet")).toBeTruthy();
  const year = screen.getByLabelText("Tax year");
  await fireEvent.input(year, { target: { value: "2032" } });
  await fireEvent.blur(year);
  expect(await screen.findByText("No records match these filters")).toBeTruthy();
  const clear = screen.getAllByRole("button", { name: "Clear filters" });
  await fireEvent.click(clear.at(-1)!);
  expect(await screen.findByText("No tax records yet")).toBeTruthy();
  expect(lastRecordQuery(fetch)).toEqual({ limit: "50", offset: "0" });
  expect((year as HTMLInputElement).value).toBe("");
});

const fullPage = Array.from({ length: 50 }, (_, index) =>
  record(`h${index + 10}`, holdings, { title: `Synthetic filing ${index}` }),
);

it("error keeps rows and offers retry", async () => {
  let fail = true;
  serveTaxes((query) =>
    query.offset === "0"
      ? { items: fullPage }
      : fail
        ? Response.json({ detail: "SENSITIVE-ZZ-123" }, { status: 503 })
        : { items: [q2], offset: 50 },
  );
  show();
  await fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Tax records could not be loaded.");
  expect(screen.getByText("Synthetic filing 0")).toBeTruthy();
  expect(document.body.textContent).not.toContain("SENSITIVE");
  fail = false;
  await fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByText("Sales tax return, Q2 2031")).toBeTruthy();
  expect(screen.queryByRole("alert")).toBeNull();
});

it("load more appears only on a full page", async () => {
  let answer!: (body: object) => void;
  serveTaxes((query) =>
    query.offset === "0" ? { items: fullPage } : new Promise((resolve) => (answer = resolve)),
  );
  show();
  const more = await screen.findByRole("button", { name: "Load more" });
  await fireEvent.click(more);
  const pending = await screen.findByRole("button", { name: "Loading…" });
  expect((pending as HTMLButtonElement).disabled).toBe(true);
  answer({ items: [q2], offset: 50 });
  expect(await screen.findByText("Sales tax return, Q2 2031")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
});

it("dates render as stored text", async () => {
  const zone = process.env.TZ;
  process.env.TZ = "Pacific/Pago_Pago";
  try {
    serveTaxes(() => ({ items: [{ ...q1, due_date: "2031-01-01" }] }));
    show();
    expect(await screen.findByText("2031-01-01")).toBeTruthy();
  } finally {
    process.env.TZ = zone;
  }
});

it("nothing tax-related is stored", async () => {
  const writes = [
    vi.spyOn(Storage.prototype, "setItem"),
    vi.spyOn(history, "pushState"),
    vi.spyOn(history, "replaceState"),
    vi.spyOn(console, "log"),
    vi.spyOn(console, "error"),
  ];
  serveTaxes(() => ({ items: [q1, notice] }));
  show();
  await screen.findByText("123400 USD");
  await fireEvent.click(await screen.findByRole("button", { name: "Example Holdings Ltd." }));
  await screen.findByRole("table", { name: "Tax records" });
  const recorded = JSON.stringify(writes.flatMap((spy) => spy.mock.calls));
  for (const value of ["Sales tax return", "123400", "5000", "Example Holdings", holdings])
    expect(recorded).not.toContain(value);
});
