import { listPeople, renamePerson, mergePerson, type PersonSummary } from "./generated/docbank.js";
import { APIError, type ProblemFace } from "./api-transport.js";
import { randomUUID } from "./crypto.js";
import type { HiddenAccess } from "./hiddenAccess.svelte.js";

export const peoplePageSize = 24;

export type MergeConflict = ProblemFace;

// A merge keeps the two selected people, the one to keep, the confirmation,
// and the exact request together, so an uncertain result retries the same
// operation.
export type PeopleMerge = {
  pair: readonly [PersonSummary, PersonSummary];
  keep?: string;
  confirmed: boolean;
  conflicts: MergeConflict[];
  notice: "" | "stale" | "uncertain";
  attempt?: { source: PersonSummary; survivor: PersonSummary; operation: string };
};

export const mergeRoles = ({ pair, keep }: PeopleMerge) => {
  const survivor = pair.find((person) => person.person_id === keep);
  const source = pair.find((person) => person.person_id !== keep);
  return survivor && source ? { source, survivor } : undefined;
};

const uncertain = (cause: unknown) => !(cause instanceof APIError) || cause.status >= 500;

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
  // read that started earlier cannot restore a card, selection, or Hidden count.
  private epoch = 0;
  private writer?: AbortController;
  private disposed = false;
  private cursorExpired = false;
  constructor(
    readonly session: string,
    readonly onauthfailure: (cause: unknown) => void,
    readonly access?: HiddenAccess,
    readonly onchanged: () => void = () => {},
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
        this.cursorExpired =
          cause instanceof APIError &&
          cause.status === 422 &&
          cause.message === "invalid people cursor";
      }
    } finally {
      if (!request.signal.aborted) this.loading = false;
    }
  }
  retry() {
    return this.load(this.query, !!this.items.length && !this.cursorExpired);
  }
  // current reads one visible person by ID, or undefined when it is hidden,
  // merged away, or gone.
  private async current(id: string, signal: AbortSignal): Promise<PersonSummary | undefined> {
    const page = await this.run(() => {
      signal.throwIfAborted();
      return listPeople(
        { person_id: id, limit: 1, photo_summary: true },
        { session: this.session, signal },
      );
    });
    return page.items[0];
  }
  private place(epoch: number, id: string, person?: PersonSummary) {
    if (epoch !== this.epoch) return;
    const replace = (rows: PersonSummary[]) =>
      person
        ? rows.map((row) => (row.person_id === id ? person : row))
        : rows.filter((row) => row.person_id !== id);
    this.items = replace(this.items);
    this.selected = replace(this.selected);
  }
  // refreshCard returns null when the read fails, leaving the card as it was.
  private async refreshCard(id: string, signal: AbortSignal, epoch: number) {
    try {
      const person = await this.current(id, signal);
      if (!signal.aborted) this.place(epoch, id, person);
      return epoch === this.epoch ? person : null;
    } catch (cause) {
      if (!signal.aborted) this.failure(cause);
      return null;
    }
  }
  private begin() {
    if (this.busy || this.disposed) return;
    const request = (this.writer = new AbortController());
    this.busy = true;
    this.writeError = "";
    return request;
  }
  async rename(person: PersonSummary, name: string) {
    const request = this.begin();
    if (!request) return false;
    const epoch = this.epoch;
    const display_name = name.trim();
    try {
      const saved = await this.run(() =>
        renamePerson(
          person.person_id,
          { display_name },
          { "If-Match": `"${person.revision}"` },
          { session: this.session, signal: request.signal },
        ),
      );
      if (request.signal.aborted) return false;
      this.place(epoch, person.person_id, {
        ...person,
        display_name: saved.display_name,
        revision: saved.revision,
        state: saved.state,
      });
      this.onchanged();
      await this.refreshCard(person.person_id, request.signal, epoch);
      return true;
    } catch (cause) {
      if (request.signal.aborted) return false;
      this.writeError = this.failure(cause);
      const current = await this.refreshCard(person.person_id, request.signal, epoch);
      // An unconfirmed rename that the current record shows has completed.
      if (uncertain(cause) && current?.display_name === display_name) {
        this.writeError = "";
        this.onchanged();
        return true;
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
    this.merge = { pair: [first, second], confirmed: false, conflicts: [], notice: "" };
  }
  closeMerge() {
    if (this.busy) return;
    this.writeError = "";
    this.merge = undefined;
  }
  keep(id: string) {
    if (!this.merge || this.busy) return;
    this.merge = { ...this.merge, keep: id, confirmed: false, notice: "" };
  }
  confirmMerge(confirmed: boolean) {
    if (this.merge && !this.busy) this.merge = { ...this.merge, confirmed };
  }
  // conflictCleared asks for a fresh confirmation after one face changes.
  conflictCleared(conflict: MergeConflict) {
    if (!this.merge) return;
    this.merge = {
      ...this.merge,
      confirmed: false,
      conflicts: this.merge.conflicts.filter((entry) => entry.face_id !== conflict.face_id),
    };
    this.onchanged();
  }
  async submitMerge() {
    const selected = this.merge;
    const roles = selected && mergeRoles(selected);
    if (!selected || !roles || !selected.confirmed) return false;
    const request = this.begin();
    if (!request) return false;
    const epoch = this.epoch;
    const { source, survivor } = roles;
    const previous = selected.attempt;
    const attempt =
      previous?.source.person_id === source.person_id &&
      previous.survivor.person_id === survivor.person_id &&
      previous.source.revision === source.revision &&
      previous.survivor.revision === survivor.revision
        ? previous
        : { source, survivor, operation: randomUUID() };
    this.merge = { ...selected, attempt, notice: "" };
    // The dialog belongs to this request while it shows the same pair.
    const owns = () => !request.signal.aborted && this.merge?.pair === selected.pair;
    try {
      await this.run(() =>
        mergePerson(
          attempt.survivor.person_id,
          {
            absorbed_person_id: attempt.source.person_id,
            absorbed_revision: attempt.source.revision,
            operation_id: attempt.operation,
          },
          { "If-Match": `"${attempt.survivor.revision}"` },
          { session: this.session, signal: request.signal },
        ),
      );
      if (request.signal.aborted) return false;
      this.merge = undefined;
      this.place(epoch, source.person_id);
      this.selected = [];
      this.onchanged();
      await this.refreshCard(survivor.person_id, request.signal, epoch);
      return true;
    } catch (cause) {
      if (!owns()) return false;
      this.writeError = this.failure(cause);
      if (uncertain(cause)) {
        this.merge = { ...this.merge!, notice: "uncertain" };
        return false;
      }
      const next: PeopleMerge = { ...this.merge!, attempt: undefined, confirmed: false };
      if (cause instanceof APIError && cause.status === 412) {
        // Show both people's current details before asking again. A failed
        // read keeps the earlier card; a person no longer visible ends the merge.
        const current = await Promise.all(
          selected.pair.map((person) => this.refreshCard(person.person_id, request.signal, epoch)),
        );
        if (!owns()) return false;
        if (current.includes(undefined)) {
          this.merge = undefined;
          this.writeError =
            "A selected person is no longer available. Choose people to merge again.";
          return false;
        }
        next.pair = [current[0] ?? selected.pair[0], current[1] ?? selected.pair[1]];
        next.notice = current.some(Boolean) ? "stale" : "";
      } else if (cause instanceof APIError && cause.code === "person_merge_conflict") {
        const notices: string[] = [];
        if (cause.message.includes("excluded or trashed photo")) {
          notices.push("An excluded or trashed photo also needs review.");
        }
        if (cause.message.includes("locked Hidden photo")) {
          notices.push("A locked Hidden photo also needs review.");
        }
        this.writeError = notices.length
          ? notices.join(" ")
          : cause.faces.length
            ? ""
            : "This merge has a face conflict that needs review.";
        next.conflicts = cause.faces;
      }
      this.merge = next;
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
