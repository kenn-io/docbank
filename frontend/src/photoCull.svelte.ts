import { createSnapshot, captureSnapshotTargets } from "./snapshots.js";
import * as api from "./generated/docbank.js";
import type { Query } from "./query.js";
import { randomUUID } from "./crypto.js";
import type {
  CullCheckpoint,
  CullProgress,
  CullRun,
  CullScope,
  CullStorage,
} from "./photoCullStorage.js";
import { photoQuery } from "./photos.svelte.js";

const scopes: CullScope[] = ["unrated", "picks", "rejects", "all"];
const failure = (cause: unknown) => (cause instanceof Error ? cause.message : String(cause));

export class PhotoCull {
  run = $state.raw<CullRun>();
  busy = $state(false);
  error = $state("");
  capturing = $state(false);
  private capture?: AbortController;
  private writes: Promise<void> = Promise.resolve();
  constructor(
    readonly session: string,
    private storage: CullStorage,
  ) {}
  get currentID() {
    return this.run?.checkpoint.pass[this.run.checkpoint.position];
  }
  get complete() {
    return !!this.run && !this.currentID;
  }
  async restore() {
    await this.writes;
    this.error = "";
    try {
      this.run = await this.storage.load();
      return !!this.run;
    } catch (cause) {
      this.error = failure(cause);
      return false;
    }
  }
  cancel() {
    this.capture?.abort();
  }
  async start(query: Query) {
    if (this.capturing) return false;
    const controller = (this.capture = new AbortController());
    this.capturing = true;
    this.error = "";
    try {
      const page = await createSnapshot(
        this.session,
        query,
        { photos: true, page_size: 250 },
        controller.signal,
      );
      const captured = await captureSnapshotTargets(this.session, page, controller.signal);
      const ids = captured.members.map((member) => {
        if (!member.asset_id) throw new Error("Photo snapshot membership was incomplete.");
        return member.asset_id;
      });
      const flags = await this.refreshFlags(ids, controller.signal);
      const run: CullRun = {
        ids,
        checkpoint: {
          runID: randomUUID(),
          pass: ids.filter((id) => flags.get(id) === ""),
          position: 0,
          visited: [],
          skipped: [],
          unavailable: ids.filter((id) => !flags.has(id)),
          scope: "unrated",
          autoAdvance: true,
        },
      };
      controller.signal.throwIfAborted();
      await this.writes;
      controller.signal.throwIfAborted();
      await this.storage.replace(run, controller.signal);
      this.run = run;
      this.visit();
      return true;
    } catch (cause) {
      if (!controller.signal.aborted) this.error = failure(cause);
      return false;
    } finally {
      this.capturing = false;
    }
  }
  private async refreshFlags(ids: string[], signal?: AbortSignal) {
    const flags = new Map<string, string>();
    for (let offset = 0; offset < ids.length; offset += 64) {
      const page = await api.listPhotoAssets(
        {
          query: {
            ...photoQuery,
            filters: { ...photoQuery.filters, asset_ids: ids.slice(offset, offset + 64) },
          },
          page_size: 250,
        },
        { session: this.session, signal },
      );
      for (const row of page.items) flags.set(row.asset_id, row.flag);
    }
    return flags;
  }
  private update(patch: Partial<CullCheckpoint>) {
    if (!this.run) return;
    const checkpoint = { ...this.run.checkpoint, ...patch };
    this.run = { ...this.run, checkpoint };
    const progress: CullProgress = patch.pass
      ? { runID: checkpoint.runID, pass: checkpoint }
      : { runID: checkpoint.runID, position: patch.position, autoAdvance: patch.autoAdvance };
    for (const kind of ["visited", "skipped", "unavailable"] as const)
      if (!patch.pass && patch[kind]?.length)
        progress.marker = { kind, id: patch[kind].at(-1)!, index: patch[kind].length - 1 };
    this.writes = this.writes
      .then(() => this.storage.checkpoint(progress))
      .catch((cause) => {
        this.error = `Progress could not be saved: ${failure(cause)}`;
      });
  }
  private visit() {
    const id = this.currentID;
    if (id && !this.run!.checkpoint.visited.includes(id))
      this.update({ visited: [...this.run!.checkpoint.visited, id] });
  }
  choose(id: string) {
    if (this.busy) return;
    const position = this.run?.checkpoint.pass.indexOf(id) ?? -1;
    if (position >= 0) {
      this.update({ position });
      this.visit();
    }
  }
  move(direction: -1 | 1) {
    if (!this.run || this.busy) return;
    this.update({
      position: Math.max(
        0,
        Math.min(this.run.checkpoint.pass.length, this.run.checkpoint.position + direction),
      ),
    });
    this.visit();
  }
  skip() {
    if (!this.currentID || this.busy) return;
    this.update({ skipped: [...new Set([...this.run!.checkpoint.skipped, this.currentID])] });
    this.move(1);
  }
  unavailable(id: string) {
    if (this.run && !this.run.checkpoint.unavailable.includes(id))
      this.update({ unavailable: [...this.run.checkpoint.unavailable, id] });
  }
  toggleAutoAdvance() {
    if (this.run) this.update({ autoAdvance: !this.run.checkpoint.autoAdvance });
  }
  async nextPass(followUp = false) {
    if (!this.run || this.busy) return;
    this.busy = true;
    this.error = "";
    try {
      const ids = followUp ? this.run.checkpoint.pass : this.run.ids;
      const flags = await this.refreshFlags(ids);
      const scope = followUp
        ? "unrated"
        : scopes[(scopes.indexOf(this.run.checkpoint.scope) + 1) % scopes.length]!;
      const pass = ids.filter(
        (id) =>
          flags.has(id) &&
          (scope === "all" ||
            flags.get(id) === (scope === "picks" ? "pick" : scope === "rejects" ? "reject" : "")),
      );
      this.update({
        scope,
        pass,
        position: 0,
        visited: [],
        skipped: [],
        unavailable: ids.filter((id) => !flags.has(id)),
      });
      this.visit();
    } catch (cause) {
      this.error = failure(cause);
    } finally {
      this.busy = false;
    }
  }
  flush() {
    return this.writes;
  }
}
