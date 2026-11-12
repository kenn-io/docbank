import { APIError } from "./api-transport.js";
import { readExactSource } from "./attachments.js";
import type { Contract, ContractDocument, ListContractsParams } from "./generated/docbank.js";
import type { RelatedSelectedSource } from "./selectedSource.js";

export const contractPageSize = 100;
export type ExpiryWindow = 0 | 30 | 90 | 365;
export interface ContractFilters {
  status: string;
  type: string;
  expiringDays: ExpiryWindow;
}
export const noContractFilters: ContractFilters = { status: "", type: "", expiringDays: 0 };

const suggestedTypes = ["service", "lease", "loan", "nda", "offer_letter"];
const statusLabels: Record<string, string> = {
  draft: "Draft",
  pending_signature: "Pending signature",
  active: "Active",
  expired: "Expired",
  terminated: "Terminated",
  cancelled: "Cancelled",
};
const roleLabels: Record<string, string> = {
  draft: "Draft",
  signed: "Signed",
  audit_trail: "Audit trail",
  amendment: "Amendment",
  attachment: "Attachment",
};

export const contractStatusOptions = [
  { value: "", label: "Any status" },
  ...Object.entries(statusLabels).map(([value, label]) => ({ value, label })),
];
export const expiryOptions: { value: string; label: string }[] = [
  { value: "0", label: "Any expiry" },
  { value: "30", label: "Within 30 days" },
  { value: "90", label: "Within 90 days" },
  { value: "365", label: "Within 365 days" },
];

function civil(date: string): Date {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date);
  if (!match) throw new Error(`Invalid date ${date}.`);
  // Date.UTC maps years 0-99 to 1900-1999; setUTCFullYear keeps the year.
  const value = new Date(0);
  value.setUTCFullYear(Number(match[1]), Number(match[2]) - 1, Number(match[3]));
  return value;
}

// addCivilDays adds whole calendar days in UTC so local midnight and daylight
// saving changes cannot shift the result.
export function addCivilDays(date: string, days: number): string {
  const value = civil(date);
  value.setUTCDate(value.getUTCDate() + days);
  return value.toISOString().slice(0, 10);
}

export function formatCivilDate(value: string | null | undefined): string {
  if (!value) return "—";
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeZone: "UTC" }).format(
    civil(value),
  );
}

export function filtersSet(filters: ContractFilters): boolean {
  return filters.status !== "" || filters.type !== "" || filters.expiringDays !== 0;
}

// contractListParams builds one page request. The window "within N days" is
// as_of through as_of+N, so the exclusive server cutoff is as_of+N+1.
export function contractListParams(
  filters: ContractFilters,
  asOf: string,
  offset: number,
): ListContractsParams {
  return {
    ...(filters.status ? { status: filters.status } : {}),
    ...(filters.type ? { type: filters.type } : {}),
    ...(filters.expiringDays && asOf
      ? { expiring_before: addCivilDays(asOf, filters.expiringDays + 1) }
      : {}),
    ...(asOf ? { as_of: asOf } : {}),
    limit: contractPageSize,
    offset,
  };
}

// visibleContracts applies the window's lower bound, which the server filter
// does not have: agreements that expired before as_of are outside the window.
export function visibleContracts(
  items: Contract[],
  filters: ContractFilters,
  asOf: string,
): Contract[] {
  if (!filters.expiringDays) return items;
  return items.filter((item) => item.expires_on !== null && item.expires_on >= asOf);
}

function words(label: string): string {
  const text = label.replaceAll("_", " ");
  return text.charAt(0).toUpperCase() + text.slice(1);
}
const anyTypeLabel = "Any type";
// Ordinary labels read as words. A label whose words would look like another
// label (stray underscores) or like the Any type filter keeps its stored form.
const readableType = /^[a-z0-9]+(?:_[a-z0-9]+)*$/;
export function contractTypeLabel(type: string): string {
  if (!type) return "—";
  if (type === "nda") return "NDA";
  const label = readableType.test(type) ? words(type) : type;
  return label === anyTypeLabel ? type : label;
}
export function contractStatusLabel(status: string): string {
  return statusLabels[status] ?? status;
}
export function documentRoleLabel(role: string): string {
  return roleLabels[role] ?? role;
}
export function statusTone(status: string): "success" | "warning" | "info" | "neutral" {
  return status === "active"
    ? "success"
    : status === "expired"
      ? "warning"
      : status === "pending_signature"
        ? "info"
        : "neutral";
}

export function counterpartyNames(contract: Contract): string {
  const names = contract.parties
    .filter((party) => party.role === "counterparty")
    .map((party) => party.display_name);
  return names.length ? names.join(", ") : "—";
}

export function contractTypeOptions(seen: Iterable<string>): { value: string; label: string }[] {
  const extra = [...new Set(seen)]
    .filter((type) => type && !suggestedTypes.includes(type))
    .map((value) => ({ value, label: contractTypeLabel(value) }))
    .sort((a, b) => a.label.localeCompare(b.label));
  return [
    { value: "", label: anyTypeLabel },
    ...suggestedTypes.map((value) => ({ value, label: contractTypeLabel(value) })),
    ...extra,
  ];
}

// contractDocumentSource opens exactly the recorded version of a link. A
// trashed, missing or changed source is refused rather than replaced.
export async function contractDocumentSource(
  session: string,
  link: ContractDocument,
  signal: AbortSignal,
): Promise<RelatedSelectedSource> {
  if (!link.available) throw new Error("This linked document is no longer available.");
  try {
    return await readExactSource(
      session,
      {
        node_id: link.node_id,
        version_id: link.content_version_id,
        sha256: link.blob_sha256,
        size: link.size,
      },
      signal,
    );
  } catch (cause) {
    if (signal.aborted) throw cause;
    if (cause instanceof APIError && cause.status === 404)
      throw new Error("This linked document is no longer available.", { cause });
    if (cause instanceof Error && cause.message.startsWith("Invalid attachment response"))
      throw new Error("This linked document no longer matches its saved version.", { cause });
    throw cause;
  }
}
