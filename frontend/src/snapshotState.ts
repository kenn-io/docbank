import { APIError } from "./api.js";
import { canonicalQuery, parseQuery, type Query } from "./query.js";
import {
  createSnapshot,
  readSnapshotPage,
  runSavedSnapshot,
  type SnapshotOptions,
  type SnapshotPage,
} from "./snapshots.js";

export interface SnapshotState {
  status: "idle" | "loading" | "ready" | "expired" | "error";
  firstPage?: SnapshotPage;
  page?: SnapshotPage;
  query?: Query;
  options?: SnapshotOptions;
  offset: number;
  error?: Error;
}

type PublishSnapshotState = (state: Readonly<SnapshotState>) => void;

function deepFreeze<T>(value: T): T {
  if (typeof value !== "object" || value === null || Object.isFrozen(value)) return value;
  for (const nested of Object.values(value)) deepFreeze(nested);
  return Object.freeze(value);
}

function copyQuery(query: Query): Query {
  return parseQuery(canonicalQuery(query));
}

function copyOptions(options: SnapshotOptions): SnapshotOptions {
  return {
    ...(options.profile === undefined ? {} : { profile: options.profile }),
    ...(options.page_size === undefined ? {} : { page_size: options.page_size }),
    ...(options.facets === undefined ? {} : { facets: [...options.facets] }),
  };
}

export class SnapshotSession {
  private state: Readonly<SnapshotState>;
  private epoch = 0;
  private request?: AbortController;
  private disposed = false;

  constructor(
    private readonly session: string,
    private readonly publish: PublishSnapshotState,
  ) {
    this.state = deepFreeze({ status: "idle", offset: 0 });
    this.publish(this.state);
  }

  async run(query: Query, options: SnapshotOptions): Promise<void> {
    const started = this.begin(query, options);
    if (started === undefined) return;
    try {
      const firstPage = await createSnapshot(this.session, started.query, started.options, started.signal);
      if (!this.current(started.epoch)) return;
      this.emit({
        status: "ready", firstPage, page: firstPage, query: started.query,
        options: started.options, offset: 0,
      });
    } catch (error) {
      this.fail(started.epoch, error, {
        status: "error", query: started.query, options: started.options, offset: 0,
      });
    }
  }

  async runSaved(
    id: string, revision: number, query: Query, options: SnapshotOptions,
  ): Promise<void> {
    const started = this.begin(query, options);
    if (started === undefined) return;
    try {
      const result = await runSavedSnapshot(
        this.session, id, revision, started.query, started.options, started.signal,
      );
      if (!this.current(started.epoch)) return;
      this.emit({
        status: "ready", firstPage: result.snapshot, page: result.snapshot,
        query: started.query, options: started.options, offset: 0,
      });
    } catch (error) {
      this.fail(started.epoch, error, {
        status: "error", query: started.query, options: started.options, offset: 0,
      });
    }
  }

  async page(direction: "previous" | "next"): Promise<boolean> {
    const accepted = this.state;
    const acceptedPage = accepted.page;
    const firstPage = accepted.firstPage;
    const query = accepted.query;
    const options = accepted.options;
    if (this.disposed || accepted.status !== "ready" || acceptedPage === undefined ||
        firstPage === undefined || query === undefined || options === undefined) return false;
    const cursor = direction === "next" ? acceptedPage.next_cursor : acceptedPage.previous_cursor;
    if (cursor === undefined) return false;
    this.request?.abort();
    const request = new AbortController();
    this.request = request;
    const epoch = ++this.epoch;
    this.emit({ ...accepted, status: "loading", error: undefined });
    try {
      const page = await readSnapshotPage(this.session, acceptedPage, cursor, request.signal);
      if (!this.current(epoch)) return false;
      const offset = direction === "next"
        ? accepted.offset + acceptedPage.rows.length
        : Math.max(0, accepted.offset - page.rows.length);
      const expected = Math.min(page.page_size, page.total - offset);
      if (offset < 0 || offset >= Math.max(page.total, 1) || page.rows.length !== expected) {
        throw new Error("Malformed snapshot receipt: page rows are inconsistent with its offset.");
      }
      this.emit({
        status: "ready", firstPage, page, query, options, offset,
      });
      return true;
    } catch (error) {
      const status = error instanceof APIError && error.status === 410 ? "expired" : "error";
      this.fail(epoch, error, {
        status, firstPage, page: acceptedPage, query, options, offset: accepted.offset,
      });
      return false;
    }
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.epoch++;
    this.request?.abort();
    this.request = undefined;
  }

  private begin(
    query: Query, options: SnapshotOptions,
  ): { epoch: number; signal: AbortSignal; query: Query; options: SnapshotOptions } | undefined {
    if (this.disposed) return undefined;
    this.request?.abort();
    const request = new AbortController();
    this.request = request;
    const epoch = ++this.epoch;
    const acceptedQuery = copyQuery(query);
    const acceptedOptions = copyOptions(options);
    this.emit({ status: "loading", query: acceptedQuery, options: acceptedOptions, offset: 0 });
    return { epoch, signal: request.signal, query: acceptedQuery, options: acceptedOptions };
  }

  private current(epoch: number): boolean {
    return !this.disposed && epoch === this.epoch;
  }

  private fail(epoch: number, caught: unknown, state: SnapshotState): void {
    if (!this.current(epoch)) return;
    const error = caught instanceof Error ? caught : new Error("Snapshot request failed.");
    this.emit({ ...state, error });
  }

  private emit(state: SnapshotState): void {
    if (this.disposed) return;
    this.state = deepFreeze(state);
    this.publish(this.state);
  }
}
