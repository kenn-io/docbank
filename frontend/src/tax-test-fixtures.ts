import { vi } from "vite-plus/test";
import type { Party, TaxRecord } from "./generated/docbank.js";

export const holdings = "11111111-1111-4111-8111-111111111111";
export const ventures = "22222222-2222-4222-8222-222222222222";
export const agency = "33333333-3333-4333-8333-333333333333";

const stamp = "2031-01-01T00:00:00Z";

export function party(
  party_id: string,
  display_name: string,
  own = true,
  retired_at: string | null = null,
): Party {
  return {
    party_id,
    display_name,
    legal_name: "",
    kind: "organization",
    own,
    country: null,
    person_id: null,
    retired_at,
    revision: 1,
    created_at: stamp,
    updated_at: stamp,
  };
}

export const parties = [
  party(holdings, "Example Holdings Ltd."),
  party(ventures, "Example Ventures Ltd."),
  party(agency, "Example Revenue Agency", false),
];

export function record(
  record_id: string,
  filer_party_id: string,
  fields: Partial<TaxRecord> = {},
): TaxRecord {
  return {
    record_id,
    filer_party_id,
    account_id: null,
    authority_party_id: null,
    kind: "filing",
    title: `Synthetic record ${record_id}`,
    form_code: "",
    country: "ZZ",
    subdivision: "",
    tax_kind: "sales-tax",
    tax_year: 2031,
    period_start: null,
    period_end: null,
    due_date: null,
    lead_days: null,
    state: "expected",
    direction: "unknown",
    amount_minor: null,
    currency: "USD",
    verification: "unverified",
    verification_note: "",
    verified_on: null,
    verified_revision: null,
    note: "",
    retired_at: null,
    revision: 1,
    created_at: stamp,
    updated_at: stamp,
    ...fields,
  };
}

export const taxQuery = (url: URL) => Object.fromEntries(url.searchParams);

type Answer = object | Response;
type Route = (query: Record<string, string>) => Answer | Promise<Answer>;

// serveTaxes answers the two reads the Taxes page makes; anything else fails.
export function serveTaxes(
  records: Route,
  partyPage: Route = () => ({ items: parties, limit: 1000 }),
) {
  return vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = new URL(String(input), location.origin);
    const route =
      url.pathname === "/api/v1/tax/records"
        ? records
        : url.pathname === "/api/v1/parties"
          ? partyPage
          : undefined;
    if (!route) throw new Error(`Unexpected request ${url.pathname}`);
    const answer = await route(taxQuery(url));
    return answer instanceof Response
      ? answer
      : Response.json({ limit: Number(url.searchParams.get("limit")), offset: 0, ...answer });
  });
}
