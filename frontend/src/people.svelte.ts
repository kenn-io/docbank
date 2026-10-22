import { listPeople, renamePerson, mergePerson, type PersonSummary } from "./generated/docbank.js";
import { APIError, type ProblemFace } from "./api-transport.js";
import { randomUUID } from "./crypto.js";
import type { HiddenAccess } from "./hiddenAccess.svelte.js";

export const peoplePageSize = 24;

export type MergeConflict = ProblemFace;

// A merge keeps its source, survivor, confirmation, and exact request together,
// so an uncertain result retries the same operation.
export type PeopleMerge = {
  source: PersonSummary;
  survivor?: PersonSummary;
  confirmed: boolean;
  conflicts: MergeConflict[];
  notice: string;
  attempt?: { source: PersonSummary; survivor: PersonSummary; operation: string };
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
  private controller?: AbortController;
  private writer?: AbortController;
  private disposed = false;
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
      if (!request.signal.aborted) this.error = this.failure(cause);
    } finally {
      if (!request.signal.aborted) this.loading = false;
    }
  }
  // current reads one visible person by ID, or undefined when it is hidden,
  // merged away, or gone.
  private async current(id: string, signal: AbortSignal) {
    const page = await this.run(() => {
      signal.throwIfAborted();
      return listPeople(
        { person_id: id, limit: 1, photo_summary: true },
        { session: this.session, signal },
      );
    });
    return page.items[0];
  }
  private place(id: string, person?: PersonSummary) {
    this.items = person
      ? this.items.map((row) => (row.person_id === id ? person : row))
      : this.items.filter((row) => row.person_id !== id);
  }
  // refreshCard returns null when the read fails, leaving the card as it was.
  private async refreshCard(id: string, signal: AbortSignal) {
    try {
      const person = await this.current(id, signal);
      if (!signal.aborted) this.place(id, person);
      return person;
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
      this.place(person.person_id, {
        ...person,
        display_name: saved.display_name,
        revision: saved.revision,
        state: saved.state,
      });
      this.onchanged();
      await this.refreshCard(person.person_id, request.signal);
      return true;
    } catch (cause) {
      if (request.signal.aborted) return false;
      this.writeError = this.failure(cause);
      const current = await this.refreshCard(person.person_id, request.signal);
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
  startMerge(source: PersonSummary) {
    if (this.busy) return;
    this.writeError = "";
    this.merge = { source, confirmed: false, conflicts: [], notice: "" };
  }
  closeMerge() {
    this.writeError = "";
    this.merge = undefined;
  }
  chooseSurvivor(survivor: PersonSummary | undefined) {
    if (!this.merge || this.busy) return;
    this.merge = { ...this.merge, survivor, confirmed: false, conflicts: [], notice: "" };
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
    const { source, survivor } = selected ?? {};
    if (!selected || !source || !survivor || !selected.confirmed) return false;
    const request = this.begin();
    if (!request) return false;
    const previous = selected.attempt;
    const attempt =
      previous?.source.person_id === source.person_id &&
      previous.survivor.person_id === survivor.person_id &&
      previous.source.revision === source.revision &&
      previous.survivor.revision === survivor.revision
        ? previous
        : { source, survivor, operation: randomUUID() };
    this.merge = { ...selected, attempt, notice: "" };
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
      this.place(source.person_id);
      this.onchanged();
      await this.refreshCard(survivor.person_id, request.signal);
      return true;
    } catch (cause) {
      if (request.signal.aborted || this.merge?.source !== source) return false;
      this.writeError = this.failure(cause);
      if (uncertain(cause)) {
        this.merge = { ...this.merge, notice: "uncertain" };
        return false;
      }
      const next: PeopleMerge = { ...this.merge, attempt: undefined, confirmed: false };
      if (cause instanceof APIError && cause.status === 412) {
        // Show both people's current names before asking again. A failed
        // read keeps the earlier selection; a person no longer visible needs
        // a new choice.
        const [currentSource, currentSurvivor] = await Promise.all([
          this.refreshCard(source.person_id, request.signal),
          this.refreshCard(survivor.person_id, request.signal),
        ]);
        if (request.signal.aborted || this.merge?.source !== source) return false;
        if (currentSource === undefined) {
          this.merge = undefined;
          return false;
        }
        next.source = currentSource ?? source;
        next.survivor = currentSurvivor === null ? survivor : currentSurvivor;
        next.notice = currentSurvivor ? "stale" : currentSurvivor === undefined ? "reselect" : "";
      } else if (cause instanceof APIError && cause.code === "person_merge_conflict") {
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
