import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { tick } from "svelte";
import ContractsPage from "./ContractsPage.svelte";
import { drawerPage } from "./drawerNavigation.js";
import { formatBytes } from "./format.js";
import type { Contract } from "./generated/docbank.js";
import {
  contract,
  contractID,
  contractLink,
  draftVersionID,
  nodeFixture,
  versionFixture,
} from "./contract-test-fixtures.js";

type Route = (url: URL) => Response | undefined | Promise<Response | undefined>;
let requests: string[] = [];
let bodies: Record<string, string> = {};

function page(items: Contract[], asOf = "2027-01-01", limit = 100, offset = 0): Response {
  return Response.json({ items, as_of: asOf, limit, offset });
}
// many returns rows with canonical UUIDs; prefix is one hex letter.
function many(count: number, prefix: string, overrides: Partial<Contract> = {}): Contract[] {
  return Array.from({ length: count }, (_, i) =>
    contract({
      contract_id: `${prefix.repeat(4)}${String(i).padStart(4, "0")}-0000-4000-8000-000000000000`,
      title: `${prefix} agreement ${i}`,
      ...overrides,
    }),
  );
}
function serve(route: Route) {
  return vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = new URL(String(input), "http://localhost");
    requests.push(url.pathname + url.search);
    if (typeof init?.body === "string") bodies[url.pathname] = init.body;
    const response = await route(url);
    if (response) return response;
    throw new Error(`unexpected ${url.pathname}${url.search}`);
  });
}
function show(onauthfailure = vi.fn()) {
  render(ContractsPage, {
    props: { session: "synthetic", onauthfailure },
    context: new Map([[drawerPage, true]]),
  });
  return onauthfailure;
}
async function choose(name: RegExp, option: string) {
  const picker = screen.getByRole("combobox", { name });
  await waitFor(() => expect(picker.hasAttribute("disabled")).toBe(false));
  await fireEvent.click(picker);
  await fireEvent.click(await screen.findByRole("option", { name: option }));
}
const list = (url: URL) => url.pathname === "/api/v1/contracts";
// matching applies the list route's status and type filters to rows.
const matching = (url: URL, rows: Contract[]) =>
  rows.filter(
    (row) =>
      (!url.searchParams.has("status") ||
        row.effective_status === url.searchParams.get("status")) &&
      (!url.searchParams.has("type") || row.contract_type === url.searchParams.get("type")),
  );

beforeEach(() => {
  requests = [];
  bodies = {};
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("contracts page", () => {
  it("shows the register in six columns with joined counterparties", async () => {
    serve((url) => (list(url) ? page([contract()]) : undefined));
    show();
    const table = await screen.findByRole("table", { name: "Contracts" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((cell) => cell.textContent?.trim()),
    ).toStrictEqual(["Title", "Type", "Counterparty", "Status", "Starts", "Expires"]);
    const row = within(table).getAllByRole("row")[1]!;
    expect(
      within(row).getByRole("button", { name: "Show contract Northwind Storage - unit lease" }),
    ).toBeTruthy();
    expect(row.textContent).toContain("Lease");
    expect(row.textContent).toContain("Northwind Storage Ltd, Riverbend Analytics LLC");
    expect(row.textContent).toContain("Active");
    expect(requests).toStrictEqual(["/api/v1/contracts?limit=100&offset=0"]);
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  });

  it("shows the empty register hint", async () => {
    serve((url) => (list(url) ? page([]) : undefined));
    show();
    expect((await screen.findByText(/No contracts yet/)).textContent).toContain(
      "docbank contract create",
    );
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("filters by status and a seen type, then clears filters", async () => {
    const rows = [
      contract(),
      contract({
        contract_id: "bbbbbbbb-0000-4000-8000-000000000000",
        title: "Software",
        contract_type: "licence",
        status: "draft",
        effective_status: "draft",
      }),
    ];
    serve((url) => (list(url) ? page(matching(url, rows)) : undefined));
    show();
    await screen.findByRole("table", { name: "Contracts" });
    await choose(/^Status/, "Active");
    await waitFor(() =>
      expect(requests.at(-1)).toBe(
        "/api/v1/contracts?status=active&as_of=2027-01-01&limit=100&offset=0",
      ),
    );
    await choose(/^Type/, "Licence");
    await waitFor(() =>
      expect(requests.at(-1)).toBe(
        "/api/v1/contracts?status=active&type=licence&as_of=2027-01-01&limit=100&offset=0",
      ),
    );
    expect(await screen.findByText("No contracts match these filters.")).toBeTruthy();
    // The empty state's own action is the only Clear filters button.
    await fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(await screen.findByRole("button", { name: "Show contract Software" })).toBeTruthy();
    expect(requests.at(-1)).toBe("/api/v1/contracts?as_of=2027-01-01&limit=100&offset=0");
  });

  it("keeps fetching hidden pages until a row is in the window or the list ends", async () => {
    const expired = many(100, "e", { expires_on: "2026-11-30", effective_status: "expired" });
    serve((url) => {
      if (!list(url)) return undefined;
      if (!url.searchParams.has("expiring_before")) return page([contract()], "2027-01-31");
      if (url.searchParams.get("offset") === "0") return page(expired, "2027-01-31");
      // Only the 30-day window has a visible row after the hidden page.
      return url.searchParams.get("expiring_before") === "2027-03-03"
        ? page([contract({ title: "Due soon", expires_on: "2027-02-10" })], "2027-01-31")
        : page([], "2027-01-31");
    });
    show();
    await screen.findByRole("table", { name: "Contracts" });
    await choose(/^Expires/, "Within 30 days");
    expect(await screen.findByRole("button", { name: "Show contract Due soon" })).toBeTruthy();
    expect(requests.slice(1)).toStrictEqual([
      "/api/v1/contracts?expiring_before=2027-03-03&as_of=2027-01-31&limit=100&offset=0",
      "/api/v1/contracts?expiring_before=2027-03-03&as_of=2027-01-31&limit=100&offset=100",
    ]);
    expect(screen.queryByText("e agreement 0")).toBeNull();
    await choose(/^Expires/, "Within 90 days");
    expect(await screen.findByText("No contracts match these filters.")).toBeTruthy();
    expect(requests.filter((r) => r.includes("expiring_before=2027-05-02"))).toHaveLength(2);
  });

  it("captions the window as its date range with or without a status", async () => {
    serve((url) => (list(url) ? page([contract({ expires_on: "2027-01-20" })]) : undefined));
    show();
    await screen.findByRole("table", { name: "Contracts" });
    await choose(/^Expires/, "Within 30 days");
    const caption = () => screen.getByText(/Shows agreements expiring/).textContent?.trim() ?? "";
    expect(caption()).toMatch(/through .*\.$/);
    expect(caption()).not.toMatch(/status/);
    const unset = caption();
    await choose(/^Status/, "Draft");
    await waitFor(() => expect(requests.at(-1)).toContain("status=draft"));
    expect(caption()).toBe(unset);
  });

  it("renews the daemon date before requesting a horizon page on refresh", async () => {
    let date = "2027-01-01";
    serve((url) => (list(url) ? page([contract()], date) : undefined));
    show();
    await screen.findByRole("table", { name: "Contracts" });
    await choose(/^Expires/, "Within 365 days");
    await waitFor(() => expect(requests).toHaveLength(2));
    date = "2027-01-02";
    await fireEvent.click(screen.getByRole("button", { name: "Refresh contracts" }));
    await waitFor(() => expect(requests).toHaveLength(4));
    expect(requests.slice(2)).toStrictEqual([
      "/api/v1/contracts?limit=1",
      "/api/v1/contracts?expiring_before=2028-01-03&as_of=2027-01-02&limit=100&offset=0",
    ]);
  });

  it("keeps rows through a failed page and shows a repeated live row once", async () => {
    const first = many(100, "a");
    const added = contract({
      contract_id: "bbbbbbbb-0000-4000-8000-000000000000",
      title: "b added",
    });
    let fail = true;
    serve((url) => {
      if (!list(url)) return undefined;
      if (url.searchParams.get("offset") === "0") return page(first);
      if (fail) {
        fail = false;
        return Response.json({ detail: "Daemon busy" }, { status: 503 });
      }
      // A concurrent insert ahead of the offset shifts the last row of the
      // first page onto the second page.
      return page([first[99]!, added]);
    });
    show();
    await screen.findByRole("button", { name: "Show contract a agreement 99" });
    await fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect((await screen.findByRole("alert")).textContent).toContain("Daemon busy");
    expect(screen.getByRole("button", { name: "Show contract a agreement 0" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByRole("button", { name: "Show contract b added" })).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Show contract a agreement 99" })).toHaveLength(1);
    expect(screen.getAllByRole("row")).toHaveLength(102);
    expect(requests.at(-1)).toBe("/api/v1/contracts?as_of=2027-01-01&limit=100&offset=100");
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  });

  it("ignores an older filter response", async () => {
    let releaseActive: (() => void) | undefined;
    let staleParsed = false;
    serve(async (url) => {
      if (!list(url)) return undefined;
      const status = url.searchParams.get("status");
      if (status === "active") {
        await new Promise<void>((resolve) => (releaseActive = resolve));
        const stale = page([contract({ title: "Stale active" })]);
        // The page handler continues in the microtasks after its body parses.
        const json = stale.json.bind(stale);
        stale.json = () =>
          json().then((value) => {
            staleParsed = true;
            return value;
          });
        return stale;
      }
      if (status === "draft") return page([contract({ title: "Fresh draft" })]);
      return page([contract()]);
    });
    show();
    await screen.findByRole("table", { name: "Contracts" });
    await choose(/^Status/, "Active");
    await waitFor(() => expect(releaseActive).toBeDefined());
    await choose(/^Status/, "Draft");
    await screen.findByRole("button", { name: "Show contract Fresh draft" });
    releaseActive?.();
    await waitFor(() => expect(staleParsed).toBe(true));
    await tick();
    expect(screen.queryByText("Stale active")).toBeNull();
    expect(screen.getByRole("button", { name: "Show contract Fresh draft" })).toBeTruthy();
  });

  it("retries after a list error", async () => {
    let calls = 0;
    serve((url) =>
      list(url)
        ? ++calls === 1
          ? Response.json({ detail: "Daemon busy" }, { status: 503 })
          : page([contract()])
        : undefined,
    );
    show();
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: /^Status/ }).hasAttribute("disabled")).toBe(true);
    await fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("table", { name: "Contracts" })).toBeTruthy();
  });

  it("reports an expired session without an alert", async () => {
    serve(() => Response.json({ detail: "Session expired" }, { status: 401 }));
    const auth = show();
    await waitFor(() => expect(auth).toHaveBeenCalledOnce());
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("contract detail", () => {
  const documents = [
    contractLink(),
    contractLink({
      role: "draft",
      node_id: 413,
      content_version_id: draftVersionID,
      blob_sha256: "b".repeat(64),
      current: false,
    }),
    contractLink({
      role: "audit_trail",
      node_id: 414,
      blob_sha256: "d".repeat(64),
      available: false,
      current: false,
      size: 4096,
    }),
    contractLink({
      role: "attachment",
      node_id: 77,
      content_version_id: "77777777-7777-4777-8777-777777777777",
    }),
  ];
  const draftNode = nodeFixture({
    id: 413,
    name: "lease-draft.txt",
    path: "/agreements/lease-draft.txt",
    current_version_id: "88888888-8888-4888-8888-888888888888",
  });

  function detailRoutes(detail: () => Response): Route {
    return (url) => {
      if (list(url)) return page([contract({ documents })]);
      if (url.pathname === `/api/v1/contracts/${contractID}`) return detail();
      if (url.pathname === "/api/v1/nodes/412") return Response.json(nodeFixture());
      if (url.pathname === "/api/v1/nodes/413") return Response.json(draftNode);
      if (url.pathname === "/api/v1/nodes/77")
        return Response.json({ detail: "Not found" }, { status: 404 });
      if (url.pathname === `/api/v1/versions/${draftVersionID}`)
        return Response.json({ ...versionFixture(documents[1]!), mime_type: "text/plain" });
      // The reader prepares the exact version it shows; the prepared bytes
      // themselves are covered by the reader's own tests.
      if (url.pathname === "/api/daemon/web-download")
        return Response.json({ detail: "Preview unavailable" }, { status: 503 });
      return undefined;
    };
  }

  it("reads the contract fresh and shows facts, parties, terms and documents", async () => {
    serve(detailRoutes(() => Response.json(contract({ documents, title: "Fresh title" }))));
    show();
    await choose(/^Status/, "Active");
    await fireEvent.click(
      await screen.findByRole("button", { name: "Show contract Northwind Storage - unit lease" }),
    );
    expect(await screen.findByRole("heading", { name: "Fresh title" })).toBeTruthy();
    expect(requests).toContain(`/api/v1/contracts/${contractID}?as_of=2027-01-01`);
    const facts = screen.getByLabelText("Contract details");
    expect(facts.textContent).toContain("Lease");
    expect(facts.textContent).toContain("Signed");
    expect(screen.getByText("Own entity")).toBeTruthy();
    expect(screen.getByText("Retired")).toBeTruthy();
    expect(screen.getByText("Monthly fee of 120 units.")).toBeTruthy();
    expect(screen.queryByText("Purpose")).toBeNull();
    expect(await screen.findByText("lease-signed.pdf")).toBeTruthy();
    expect(await screen.findByText("lease-draft.txt")).toBeTruthy();
    expect(await screen.findByText("id:77")).toBeTruthy();
    expect(screen.getByText("Older version")).toBeTruthy();
    expect(screen.getByText("Unavailable")).toBeTruthy();
    // An unavailable link shows its whole recorded hash and size as evidence.
    const hash = "d".repeat(64);
    const evidence = screen.getByText(
      (_, element) =>
        !!element?.textContent?.includes(hash) &&
        ![...element.children].some((child) => child.textContent?.includes(hash)),
    );
    expect(evidence.textContent).toContain(`sha256 ${hash}`);
    expect(evidence.closest("li")?.textContent).toContain(formatBytes(4096));
    expect(
      screen.getByRole("button", { name: "Open Audit trail id:414" }).hasAttribute("disabled"),
    ).toBe(true);
    expect(requests).not.toContain("/api/v1/nodes/414");

    cleanup();
    vi.restoreAllMocks();
    const empty = contract({
      parties: [],
      documents: [],
      summary: "",
      consideration: "",
      termination: "",
    });
    serve(detailRoutes(() => Response.json(empty)));
    show();
    await fireEvent.click(await screen.findByRole("button", { name: /^Show contract/ }));
    expect(await screen.findByText("No parties linked.")).toBeTruthy();
    expect(screen.getByText("No terms recorded.")).toBeTruthy();
    expect(screen.getByText("No documents linked.")).toBeTruthy();
  });

  it("returns to the table with focus on the title and the filters kept", async () => {
    serve(detailRoutes(() => Response.json(contract({ documents }))));
    show();
    await choose(/^Status/, "Active");
    await fireEvent.click(
      await screen.findByRole("button", { name: "Show contract Northwind Storage - unit lease" }),
    );
    await screen.findByRole("heading", { name: "Northwind Storage - unit lease" });
    await fireEvent.click(screen.getByRole("button", { name: "All contracts" }));
    await waitFor(() =>
      expect(document.activeElement?.getAttribute("aria-label")).toBe(
        "Show contract Northwind Storage - unit lease",
      ),
    );
    expect(screen.getByRole("combobox", { name: /^Status/ }).textContent).toContain("Active");
  });

  it("opens the exact older version in the reader and returns focus", async () => {
    serve(detailRoutes(() => Response.json(contract({ documents }))));
    show();
    await fireEvent.click(await screen.findByRole("button", { name: /^Show contract/ }));
    const open = await screen.findByRole("button", { name: "Open Draft lease-draft.txt" });
    await fireEvent.click(open);
    const reader = await screen.findByRole("dialog", { name: "Read lease-draft.txt" });
    await waitFor(() => expect(bodies["/api/daemon/web-download"]).toBeDefined());
    expect(JSON.parse(bodies["/api/daemon/web-download"]!)).toMatchObject({
      node_id: 413,
      version_id: draftVersionID,
      blob_hash: "b".repeat(64),
      size: 2048,
    });
    expect(draftNode.current_version_id).not.toBe(draftVersionID);
    await fireEvent.click(within(reader).getAllByRole("button", { name: /Close/ })[0]!);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() =>
      expect(document.activeElement?.getAttribute("aria-label")).toBe("Open Draft lease-draft.txt"),
    );
    expect(screen.getByRole("heading", { name: "Northwind Storage - unit lease" })).toBeTruthy();
  });

  it("refuses a link whose version no longer matches", async () => {
    const routes = detailRoutes(() => Response.json(contract({ documents })));
    serve((url) =>
      url.pathname === `/api/v1/versions/${draftVersionID}`
        ? Response.json({ ...versionFixture(documents[1]!), size: 1 })
        : routes(url),
    );
    show();
    await fireEvent.click(await screen.findByRole("button", { name: /^Show contract/ }));
    await fireEvent.click(
      await screen.findByRole("button", { name: "Open Draft lease-draft.txt" }),
    );
    expect((await screen.findByRole("alert")).textContent).toContain("no longer matches");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("refreshes the open contract through a failed list and its deletion", async () => {
    let listStatus = 200;
    let detailStatus = 200;
    const routes = detailRoutes(() =>
      detailStatus === 200
        ? Response.json(contract({ documents: [] }))
        : Response.json({ detail: "Not found" }, { status: detailStatus }),
    );
    serve((url) =>
      list(url) && listStatus !== 200
        ? Response.json({ detail: "Daemon busy" }, { status: listStatus })
        : routes(url),
    );
    show();
    await fireEvent.click(await screen.findByRole("button", { name: /^Show contract/ }));
    await screen.findByRole("heading", { name: "Northwind Storage - unit lease" });
    listStatus = 500;
    await fireEvent.click(screen.getByRole("button", { name: "Refresh contracts" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Daemon busy");
    expect(screen.getByRole("heading", { name: "Northwind Storage - unit lease" })).toBeTruthy();
    listStatus = 200;
    await fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(
      await screen.findByRole("heading", { name: "Northwind Storage - unit lease" }),
    ).toBeTruthy();
    detailStatus = 404;
    await fireEvent.click(screen.getByRole("button", { name: "Refresh contracts" }));
    expect(await screen.findByText("This contract no longer exists.")).toBeTruthy();
    await fireEvent.click(screen.getAllByRole("button", { name: "All contracts" }).at(-1)!);
    expect(await screen.findByRole("table", { name: "Contracts" })).toBeTruthy();
  });
});
