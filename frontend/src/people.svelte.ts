import {
  listPeople,
  renamePerson,
  mergePerson,
  type Person,
  type PersonMergeReceipt,
  type PersonSummary,
} from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { randomUUID } from "./crypto.js";
import { uuidV4Pattern as uuid } from "./query.js";
import type { HiddenAccess } from "./hiddenAccess.svelte.js";

async function readPerson(response: Response, expectedID: string): Promise<Person> {
  const person = await response.json();
  if (
    !person ||
    person.person_id !== expectedID ||
    typeof person.display_name !== "string" ||
    !Number.isSafeInteger(person.revision) ||
    person.revision < 1 ||
    response.headers.get("ETag") !== `"${person.revision}"`
  )
    throw new Error("Invalid person response.");
  return person;
}

async function readReceipt(
  response: Response,
  source: PersonSummary,
  survivor: PersonSummary,
  operationID: string,
): Promise<PersonMergeReceipt> {
  const receipt = await response.json();
  if (
    !receipt ||
    typeof receipt.merge_id !== "string" ||
    !uuid.test(receipt.merge_id) ||
    receipt.operation_id !== operationID ||
    receipt.absorbed_person_id !== source.person_id ||
    receipt.survivor_person_id !== survivor.person_id ||
    response.headers.get("ETag") !== `"${receipt.survivor_revision_after}"`
  )
    throw new Error("Invalid person merge response.");
  return receipt;
}

export const peoplePageSize = 24;
export type PeopleMerge = {
  pair: readonly [PersonSummary, PersonSummary];
  keep?: string;
  confirmed: boolean;
};
export const mergeRoles = ({ pair, keep }: PeopleMerge) => {
  const survivor = pair.find((person) => person.person_id === keep);
  const source = pair.find((person) => person.person_id !== keep);
  return survivor && source ? { source, survivor } : undefined;
};

export class People {
  items = $state.raw<PersonSummary[]>([]);
  // draft is the search box text; query is the search the loaded cards answer.
  draft = $state("");
  query = $state("");
  cursor = $state<string>();
  loading = $state(false);
  error = $state("");
  busy = $state(false);
  writeError = $state("");
  merge = $state.raw<PeopleMerge>();
  // selected holds up to two people to merge and survives new searches.
  selected = $state.raw<PersonSummary[]>([]);
  private controller?: AbortController;
  // epoch changes whenever the cards are concealed, as when Hidden locks, so a
  // response from an earlier scope cannot restore cards or selections.
  private epoch = 0;
  private writer?: AbortController;
  private disposed = false;
  private cursorExpired = false;
  constructor(
    readonly session: string,
    readonly onauthfailure: (cause: unknown) => void,
    readonly access?: HiddenAccess,
    readonly onchanged: (id: string, name: string, survivorID?: string) => void = () => {},
  ) {}
  run<T>(request: () => Promise<T>) {
    return this.access ? this.access.run(request) : request();
  }
  failure(cause: unknown) {
    if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
    return cause instanceof Error ? cause.message : String(cause);
  }
  conceal() {
    this.controller?.abort();
    this.epoch++;
    this.selected = [];
    this.items = [];
    this.cursor = undefined;
    this.loading = false;
    this.error = "";
    this.merge = undefined;
    this.writeError = "";
  }
  async load(query = this.query, more = false) {
    if (this.disposed) return;
    if (this.access?.lockError) {
      this.conceal();
      return;
    }
    this.controller?.abort();
    const request = (this.controller = new AbortController());
    const cursor = more ? this.cursor : undefined;
    this.query = query;
    if (!more) {
      this.items = [];
      this.cursor = undefined;
    }
    this.loading = true;
    this.error = "";
    this.cursorExpired = false;
    try {
      const page = await this.run(() => {
        request.signal.throwIfAborted();
        return listPeople(
          { query, limit: peoplePageSize, photo_summary: true, ...(cursor ? { cursor } : {}) },
          { session: this.session, signal: request.signal },
        );
      });
      if (!request.signal.aborted) {
        this.items = [
          ...new Map(
            [...this.items, ...page.items].map((person) => [person.person_id, person]),
          ).values(),
        ];
        this.cursor = page.next_cursor;
      }
    } catch (cause) {
      if (!request.signal.aborted) {
        this.error = this.failure(cause);
        this.cursorExpired = cause instanceof APIError && cause.code === "people_cursor_expired";
      }
    } finally {
      if (!request.signal.aborted) this.loading = false;
    }
  }
  retry() {
    return this.load(this.query, !!this.items.length && !this.cursorExpired);
  }
  private begin() {
    if (this.busy || this.disposed) return;
    const request = (this.writer = new AbortController());
    this.busy = true;
    this.writeError = "";
    return request;
  }
  // Uncertain writes reload the cards and require fresh selections. Rejected
  // input keeps the cards; stale or missing people also clear the selection.
  private async recover(cause: unknown, epoch: number, person?: PersonSummary, name?: string) {
    const message = this.failure(cause);
    const status = cause instanceof APIError ? cause.status : 0;
    if (epoch === this.epoch) {
      this.writeError = message;
      if (
        status === 404 ||
        status === 412 ||
        (cause instanceof APIError && cause.code === "person_retired")
      )
        this.selected = [];
      if (status !== 422) await this.load();
    }
    if (status === 0 || status >= 500) {
      if (epoch === this.epoch) this.selected = [];
      if (!person || name === undefined) return;
      try {
        let saved: PersonSummary | undefined;
        let cursor: string | undefined;
        do {
          if (this.writer?.signal.aborted) return;
          const page = await this.run(() =>
            listPeople(
              {
                query: name,
                limit: peoplePageSize,
                photo_summary: true,
                ...(cursor ? { cursor } : {}),
              },
              { session: this.session, signal: this.writer?.signal },
            ),
          );
          if (this.writer?.signal.aborted) return;
          saved ??= page.items.find(
            (row) =>
              row.person_id === person.person_id &&
              row.display_name === name &&
              row.revision > person.revision,
          );
          cursor = page.next_cursor;
        } while (cursor);
        if (saved) this.onchanged(person.person_id, saved.display_name);
      } catch (recoveryCause) {
        this.failure(recoveryCause);
      }
    }
  }
  async rename(person: PersonSummary, name: string) {
    const request = this.begin();
    if (!request) return false;
    const epoch = this.epoch;
    try {
      const response = await this.run(() =>
        renamePerson(
          person.person_id,
          { display_name: name.trim() },
          { "If-Match": `"${person.revision}"` },
          { session: this.session, signal: request.signal },
        ),
      );
      const saved = await readPerson(response, person.person_id);
      if (request.signal.aborted) return false;
      this.onchanged(person.person_id, saved.display_name);
      if (epoch === this.epoch) {
        const replace = (rows: PersonSummary[]) =>
          rows.map((row) =>
            row.person_id === person.person_id
              ? { ...row, display_name: saved.display_name, revision: saved.revision }
              : row,
          );
        this.selected = replace(this.selected);
        await this.load();
      }
      return true;
    } catch (cause) {
      if (!request.signal.aborted) {
        await this.recover(cause, epoch, person, name.trim());
      }
      return false;
    } finally {
      if (!request.signal.aborted) this.busy = false;
    }
  }
  toggle(person: PersonSummary) {
    if (this.merge) return;
    this.writeError = "";
    if (this.selected.some((row) => row.person_id === person.person_id))
      this.selected = this.selected.filter((row) => row.person_id !== person.person_id);
    else if (this.selected.length < 2) this.selected = [...this.selected, person];
  }
  clearSelection() {
    if (this.merge) return;
    this.selected = [];
    this.writeError = "";
  }
  startMerge() {
    const [first, second] = this.selected;
    if (this.busy || !first || !second) return;
    this.writeError = "";
    this.merge = { pair: [first, second], confirmed: false };
  }
  closeMerge() {
    if (this.busy) return;
    this.writeError = "";
    this.merge = undefined;
  }
  keep(id: string) {
    if (!this.merge || this.busy) return;
    this.merge = { ...this.merge, keep: id, confirmed: false };
  }
  confirmMerge(confirmed: boolean) {
    if (this.merge && !this.busy) this.merge = { ...this.merge, confirmed };
  }
  async submitMerge() {
    const selected = this.merge;
    const roles = selected && mergeRoles(selected);
    if (!selected || !roles || !selected.confirmed) return false;
    const request = this.begin();
    if (!request) return false;
    const epoch = this.epoch;
    const { source, survivor } = roles;
    const operationID = randomUUID();
    const send = async () => {
      const response = await this.run(() =>
        mergePerson(
          survivor.person_id,
          {
            absorbed_person_id: source.person_id,
            absorbed_revision: source.revision,
            operation_id: operationID,
          },
          { "If-Match": `"${survivor.revision}"` },
          { session: this.session, signal: request.signal },
        ),
      );
      await readReceipt(response, source, survivor, operationID);
    };
    try {
      await send().catch((cause: unknown) => {
        const status = cause instanceof APIError ? cause.status : 0;
        if (request.signal.aborted || (status !== 0 && status < 500)) throw cause;
        return send();
      });
      if (request.signal.aborted) return false;
      this.onchanged(source.person_id, survivor.display_name, survivor.person_id);
      if (epoch === this.epoch) {
        this.merge = undefined;
        this.selected = [];
        await this.load();
      }
      return true;
    } catch (cause) {
      if (!request.signal.aborted) {
        if (epoch === this.epoch) this.merge = undefined;
        await this.recover(cause, epoch);
      }
      return false;
    } finally {
      if (!request.signal.aborted) this.busy = false;
    }
  }
  dispose() {
    this.disposed = true;
    this.conceal();
    this.writer?.abort();
  }
}
