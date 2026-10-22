import { listPeople, renamePerson, mergePerson, type PersonSummary } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { randomUUID } from "./crypto.js";
import type { HiddenAccess } from "./hiddenAccess.svelte.js";

let recentSession = "";
let recentIDs: string[] = [];
export function recentPeople(session: string) {
  if (session !== recentSession) {
    recentSession = session;
    recentIDs = [];
  }
  return [...recentIDs];
}
export function recordRecentPerson(session: string, id: string) {
  recentIDs = [id, ...recentPeople(session).filter((other) => other !== id)].slice(0, 20);
}
export function clearRecentPeople() {
  recentSession = "";
  recentIDs = [];
}

export class People {
  items = $state.raw<PersonSummary[]>([]);
  query = $state("");
  cursor = $state<string>();
  loading = $state(false);
  error = $state("");
  busy = $state(false);
  writeError = $state("");
  uncertain = $state(false);
  private controller?: AbortController;
  private writer?: AbortController;
  private disposed = false;
  private mergeAttempt?: { source: PersonSummary; target: PersonSummary; operation: string };
  constructor(
    readonly session: string,
    readonly onauthfailure: (cause: unknown) => void,
    readonly access?: HiddenAccess,
    readonly onchanged: () => void = () => {},
  ) {}
  private run<T>(request: () => Promise<T>) {
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
          { query, limit: 100, photo_summary: true, ...(cursor ? { cursor } : {}) },
          { session: this.session, signal: request.signal },
        );
      });
      if (!request.signal.aborted) {
        this.items = [...this.items, ...page.items];
        this.cursor = page.next_cursor;
      }
    } catch (cause) {
      if (!request.signal.aborted) this.error = this.failure(cause);
    } finally {
      if (!request.signal.aborted) this.loading = false;
    }
  }
  async rename(person: PersonSummary, name: string) {
    if (this.busy || this.disposed) return false;
    const request = (this.writer = new AbortController());
    this.busy = true;
    this.writeError = "";
    try {
      await this.run(() =>
        renamePerson(
          person.person_id,
          { display_name: name.trim() },
          { "If-Match": `"${person.revision}"` },
          { session: this.session, signal: request.signal },
        ),
      );
      if (request.signal.aborted) return false;
      this.onchanged();
      await this.load();
      return true;
    } catch (cause) {
      if (!request.signal.aborted) {
        this.writeError = this.failure(cause);
        this.onchanged();
        await this.load();
        if (!(cause instanceof APIError) || cause.status >= 500) {
          if (
            this.items.some(
              (row) => row.person_id === person.person_id && row.display_name === name.trim(),
            )
          )
            return true;
          try {
            let cursor: string | undefined;
            do {
              const page = await this.run(() => {
                request.signal.throwIfAborted();
                return listPeople(
                  { query: name.trim(), limit: 100, ...(cursor ? { cursor } : {}) },
                  { session: this.session, signal: request.signal },
                );
              });
              if (request.signal.aborted) return false;
              if (
                page.items.some(
                  (row) => row.person_id === person.person_id && row.display_name === name.trim(),
                )
              )
                return true;
              cursor = page.next_cursor;
            } while (cursor);
          } catch (readError) {
            if (!request.signal.aborted) this.error = this.failure(readError);
          }
        }
      }
      return false;
    } finally {
      if (!request.signal.aborted) this.busy = false;
    }
  }
  async merge(source: PersonSummary, target: PersonSummary) {
    if (this.busy || this.disposed || source.person_id === target.person_id) return false;
    if (
      this.mergeAttempt?.source.person_id !== source.person_id ||
      this.mergeAttempt.target.person_id !== target.person_id
    )
      this.mergeAttempt = { source, target, operation: randomUUID() };
    const attempt = this.mergeAttempt;
    const request = (this.writer = new AbortController());
    this.busy = true;
    this.writeError = "";
    try {
      await this.run(() =>
        mergePerson(
          attempt.target.person_id,
          {
            absorbed_person_id: attempt.source.person_id,
            absorbed_revision: attempt.source.revision,
            operation_id: attempt.operation,
          },
          { "If-Match": `"${attempt.target.revision}"` },
          { session: this.session, signal: request.signal },
        ),
      );
      if (request.signal.aborted) return false;
      this.mergeAttempt = undefined;
      this.uncertain = false;
      this.onchanged();
      await this.load();
      return true;
    } catch (cause) {
      if (!request.signal.aborted) {
        this.uncertain = !(cause instanceof APIError) || cause.status >= 500;
        if (!this.uncertain) this.mergeAttempt = undefined;
        this.writeError = this.failure(cause);
        this.onchanged();
        await this.load();
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
