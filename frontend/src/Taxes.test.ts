import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import Taxes from "./Taxes.svelte";
import { drawerPage } from "./drawerNavigation.js";
import { holdings, parties, record, serveTaxes, taxQuery, ventures } from "./tax-test-fixtures.js";

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
const recordQueries = (fetch: ReturnType<typeof serveTaxes>) =>
  fetch.mock.calls
    .map(([input]) => new URL(String(input), location.origin))
    .filter((url) => url.pathname === "/api/v1/tax/records")
    .map(taxQuery);

it("shows loading, then grouped rows with state, due and exact amount", async () => {
  let answer!: (body: object) => void;
  serveTaxes(() => new Promise((resolve) => (answer = resolve)));
  show();
  expect(await screen.findByText(/Loading…/)).toBeTruthy();
  // Ventures' record comes first from the server; headings still sort by name.
  answer({ items: [notice, q1, q2] });
  const first = await screen.findByRole("table", { name: "Tax records for Example Holdings Ltd." });
  const headings = screen.getAllByRole("heading", { level: 3 });
  expect(headings.map((heading) => heading.textContent)).toEqual([
    "Example Holdings Ltd.",
    "Example Ventures Ltd.",
  ]);
  expect(headings.some((heading) => /\d/.test(heading.textContent ?? ""))).toBe(false);
  const row = within(first).getAllByRole("row")[1]!;
  for (const text of [
    "Form 100",
    "2031",
    "2031-04-30",
    "Expected",
    "123400 USD",
    "Direction: Payable",
  ])
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
  expect(recordQueries(fetch).some((query) => query.filer === ventures)).toBe(true);
});

it("unsafe amount shows the row's CLI command as visible text", async () => {
  const hint = "Run docbank tax record show h1 to see the exact amount";
  serveTaxes(() => ({ items: [{ ...q1, amount_minor: 9007199254740992 }] }));
  show();
  const cell = (await screen.findByText("Too large to show")).closest("td")!;
  const shown = within(cell).getByText(hint);
  expect(shown.classList.contains("kit-sr-only")).toBe(false);
  expect(cell.title).toBe("");
});

it("year field applies valid years and rejects others without a request", async () => {
  const fetch = serveTaxes(() => ({ items: [q1] }));
  show();
  await screen.findByText("Sales tax return, Q1 2031");
  const year = screen.getByLabelText("Tax year");
  await fireEvent.input(year, { target: { value: "2031" } });
  await fireEvent.keyDown(year, { key: "Enter" });
  await waitFor(() =>
    expect(recordQueries(fetch).some((query) => query.year === "2031")).toBe(true),
  );
  for (const value of ["0", "10000", "20x1"]) {
    const requests = fetch.mock.calls.length;
    await fireEvent.input(year, { target: { value } });
    await fireEvent.keyDown(year, { key: "Enter" });
    const error = screen.getByRole("alert");
    expect(error.textContent).toBe("Enter a year from 1 to 9999");
    expect(year.getAttribute("aria-describedby")).toBe(error.id);
    expect(fetch.mock.calls.length).toBe(requests);
  }
  await fireEvent.click(screen.getByRole("button", { name: "All years" }));
  await waitFor(() => expect(recordQueries(fetch).at(-1)).not.toHaveProperty("year"));
  expect(screen.queryByText("Enter a year from 1 to 9999")).toBeNull();
  expect(year.getAttribute("aria-describedby")).toBeNull();
});

it("with a filer, All years keeps it and Clear filters removes both", async () => {
  const fetch = serveTaxes((query) => ({ items: query.filer === ventures ? [notice] : [q1] }));
  show();
  const rail = screen.getByRole("navigation", { name: "Tax filters" });
  await fireEvent.click(await within(rail).findByRole("button", { name: "Example Ventures Ltd." }));
  await screen.findByRole("table", { name: "Tax records" });
  expect(screen.getByRole("button", { name: "Clear filters" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "All years" })).toBeNull();
  const year = screen.getByLabelText("Tax year");
  const applyYear = async () => {
    await fireEvent.input(year, { target: { value: "2031" } });
    await fireEvent.keyDown(year, { key: "Enter" });
    await screen.findByRole("button", { name: "All years" });
  };
  await applyYear();
  expect(screen.getByRole("button", { name: "Clear filters" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "All years" }));
  await waitFor(() => expect(recordQueries(fetch).at(-1)).not.toHaveProperty("year"));
  expect(recordQueries(fetch).at(-1)?.filer).toBe(ventures);
  expect(screen.getByRole("button", { name: "Clear filters" })).toBeTruthy();
  await applyYear();
  await fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(recordQueries(fetch).at(-1)).toEqual({ limit: "50", offset: "0" }));
  expect((year as HTMLInputElement).value).toBe("");
});

it("empty vault and no-match states", async () => {
  const fetch = serveTaxes(() => ({ items: [] }));
  show();
  expect(await screen.findByText("No tax records yet")).toBeTruthy();
  const year = screen.getByLabelText("Tax year");
  await fireEvent.input(year, { target: { value: "2032" } });
  await fireEvent.blur(year);
  expect(await screen.findByText("No records match these filters")).toBeTruthy();
  // With only a year set there is one reset: All years.
  expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "All years" }));
  expect(await screen.findByText("No tax records yet")).toBeTruthy();
  expect(recordQueries(fetch).at(-1)).not.toHaveProperty("year");
  expect((year as HTMLInputElement).value).toBe("");
});

const fullPage = Array.from({ length: 50 }, (_, index) =>
  record(`h${index + 10}`, holdings, { title: `Synthetic filing ${index}` }),
);

it("Try again retries a failed read", async () => {
  let fail = true;
  serveTaxes((query) =>
    query.offset === "0"
      ? { items: fullPage }
      : fail
        ? new Response("{}", { status: 503 })
        : { items: [q2], offset: 50 },
  );
  show();
  await fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
  await screen.findByRole("alert");
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

it("failed filer names stay distinct and can be reloaded", async () => {
  let fail = true;
  serveTaxes(
    () => ({ items: [q1, notice] }),
    () => (fail ? new Response("{}", { status: 500 }) : { items: parties, limit: 1000 }),
  );
  show();
  await screen.findByRole("table", { name: `Tax records for Filer ${ventures}` });
  expect(screen.getByRole("table", { name: `Tax records for Filer ${holdings}` })).toBeTruthy();
  expect(screen.getByRole("alert").textContent).toBe("Filers could not be loaded.");
  fail = false;
  await fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(
    await screen.findByRole("table", { name: "Tax records for Example Ventures Ltd." }),
  ).toBeTruthy();
  expect(screen.queryByRole("alert")).toBeNull();
});
