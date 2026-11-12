import {
  listParties,
  listTaxRecords,
  type ListTaxRecordsParams,
  type Party,
  type TaxRecord,
} from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

export const taxPageSize = 50;
const partyPageSize = 1000;
export type TaxFilters = { filer: string; year: number | null };
export type FilerGroup<T> = { filerPartyID: string; rows: T[] };

// recordParams is the only builder of record list requests, so every page
// answers the filters the rail shows.
export function recordParams(f: TaxFilters, offset: number): ListTaxRecordsParams {
  return {
    limit: taxPageSize,
    offset,
    ...(f.filer ? { filer: f.filer } : {}),
    ...(f.year !== null ? { year: f.year } : {}),
  };
}

// groupByFiler keeps server order: groups by each filer's first row, rows
// within a group as listed. It counts nothing.
export function groupByFiler<T extends { filer_party_id: string }>(
  rows: readonly T[],
): FilerGroup<T>[] {
  const groups = new Map<string, T[]>();
  for (const row of rows) {
    const group = groups.get(row.filer_party_id);
    if (group) group.push(row);
    else groups.set(row.filer_party_id, [row]);
  }
  return [...groups].map(([filerPartyID, rows]) => ({ filerPartyID, rows }));
}

// TaxesView owns the Taxes page's reads. Errors are fixed text because tax
// diagnostics are private (spec D29); nothing is logged or stored.
export class TaxesView {
  filters = $state<TaxFilters>({ filer: "", year: null });
  rows = $state.raw<TaxRecord[]>([]);
  groups = $derived(groupByFiler(this.rows));
  parties = $state.raw<Map<string, Party>>(new Map());
  // The rail lists current own parties plus every filer that the loaded rows
  // or the selection reference, so a filer that is no longer own (or whose
  // name failed to load) stays selectable, including while its rows reload.
  filers = $derived.by(() => {
    const ids = new Set([...this.parties.values()].filter((p) => p.own).map((p) => p.party_id));
    for (const row of this.rows) ids.add(row.filer_party_id);
    if (this.filters.filer) ids.add(this.filters.filer);
    return [...ids]
      .map((id) => ({
        party_id: id,
        name: this.filerName(id),
        retired: !!this.parties.get(id)?.retired_at,
      }))
      .sort((a, b) => Number(a.retired) - Number(b.retired) || a.name.localeCompare(b.name));
  });
  loading = $state(false);
  more = $state(false);
  error = $state("");
  partyError = $state("");
  nextOffset = $state<number | null>(null);
  private controller?: AbortController;
  private partyController?: AbortController;
  private failedOffset = 0;
  private signedOut = false;
  private disposed = false;
  constructor(
    readonly session: string,
    readonly onauthfailure: (cause: unknown) => void,
  ) {}

  load(): Promise<void> {
    return Promise.all([this.loadParties(), this.page(0)]).then(() => {});
  }
  setFilters(f: TaxFilters): Promise<void> {
    this.filters = { ...f };
    this.rows = [];
    return this.page(0);
  }
  clear(): Promise<void> {
    return this.setFilters({ filer: "", year: null });
  }
  loadMore(): Promise<void> {
    if (this.nextOffset === null || this.loading || this.more) return Promise.resolve();
    return this.page(this.nextOffset);
  }
  retry(): Promise<void> {
    return this.page(this.failedOffset);
  }
  // A filer whose party could not be read is labelled by its complete ID.
  filerName(id: string): string {
    return this.parties.get(id)?.display_name ?? `Filer ${id}`;
  }
  dispose(): void {
    this.disposed = true;
    this.controller?.abort();
    this.partyController?.abort();
  }

  private failure(cause: unknown, text: string): string {
    if (cause instanceof APIError && cause.status === 401 && !this.signedOut) {
      this.signedOut = true;
      this.onauthfailure(cause);
    }
    return text;
  }

  private async page(offset: number): Promise<void> {
    if (this.disposed) return;
    this.controller?.abort();
    const request = (this.controller = new AbortController());
    this.loading = offset === 0;
    this.more = offset > 0;
    this.error = "";
    // A first-page read replaces the list, so no later page may append to it
    // until that read succeeds; a failed refresh keeps rows but not paging.
    if (offset === 0) this.nextOffset = null;
    try {
      const page = await listTaxRecords(recordParams(this.filters, offset), {
        session: this.session,
        signal: request.signal,
      });
      if (request.signal.aborted) return;
      const kept = offset === 0 ? [] : this.rows;
      this.rows = [
        ...new Map([...kept, ...page.items].map((row) => [row.record_id, row])).values(),
      ];
      this.nextOffset = page.items.length === taxPageSize ? page.offset + page.items.length : null;
    } catch (cause) {
      if (request.signal.aborted) return;
      this.failedOffset = offset;
      this.error = this.failure(cause, "Tax records could not be loaded.");
    } finally {
      if (!request.signal.aborted) {
        this.loading = false;
        this.more = false;
      }
    }
  }

  async loadParties(): Promise<void> {
    if (this.disposed) return;
    this.partyController?.abort();
    const request = (this.partyController = new AbortController());
    this.partyError = "";
    const parties = new Map<string, Party>();
    try {
      for (let offset = 0; ; offset += partyPageSize) {
        const page = await listParties(
          { include_retired: true, limit: partyPageSize, offset },
          { session: this.session, signal: request.signal },
        );
        for (const party of page.items) parties.set(party.party_id, party);
        if (page.items.length < partyPageSize) break;
      }
      if (!request.signal.aborted) this.parties = parties;
    } catch (cause) {
      if (!request.signal.aborted)
        this.partyError = this.failure(cause, "Filers could not be loaded.");
    }
  }
}
