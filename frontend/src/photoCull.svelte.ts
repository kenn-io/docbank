import { createSnapshot, captureSnapshotTargets } from "./snapshots.js";
import type { Query } from "./query.js";
import { randomUUID } from "./crypto.js";
import { APIError } from "./api-transport.js";
import { CullFormatError, sameCheckpoint } from "./photoCullStorage.js";
import type {
  CullCheckpoint,
  CullProgress,
  CullRun,
  CullScope,
  CullStorage,
} from "./photoCullStorage.js";
import { lookupPhotoRows } from "./photos.svelte.js";
import {
  getPhotoSettings,
  groupPhotoStacks,
  type PhotoBrowseRowFlag,
} from "./generated/docbank.js";

const scopes: CullScope[] = ["unflagged", "picks", "rejects", "all"];
const failure = (cause: unknown) => (cause instanceof Error ? cause.message : String(cause));

export class PhotoCull {
  run = $state.raw<CullRun & { groups?: string[][]; burstGapSeconds?: number }>();
  private stackIndex = new WeakMap<string[][], ReadonlyMap<string, string[]>>();
  private stackRead?: AbortController;
  private passGroups?: { pass: string[]; source?: string[][]; groups: string[][] };
  stackError = $state("");
  stackLoading = $state(false);
  busy = $state(false);
  error = $state("");
  capturing = $state(false);
  stale = $state(false);
  progress = $state("");
  remainder = $state(false);
  checkingRemainder = $state(false);
  // The pass whose remainder was last read; flag edits clear it so the next completion reads again.
  private remainderPass?: string[];
  private capture?: AbortController;
  private flagRead?: AbortController;
  saved = $state(false);
  async probe() {
    try {
      this.saved = !!(await this.storage.saved());
    } catch (cause) {
      this.saved = false;
      if (cause instanceof CullFormatError) this.error = failure(cause);
    }
  }
  // nextPass snapshots the checkpoint revision, so no other update may bump it until the pass is saved.
  private changingPass = false;
  // Sets of the ids in each marker list, so a new mark costs O(1) however large the query is.
  private marked = new WeakMap<string[], Set<string>>();
  private writes: Promise<void> = Promise.resolve();
  constructor(
    readonly session: string,
    private storage: CullStorage,
    private onauthfailure?: (cause: unknown) => void,
  ) {}
  private sessionFailure(cause: unknown) {
    if (cause instanceof APIError && cause.status === 401) this.onauthfailure?.(cause);
    return failure(cause);
  }
  private apiFailure(cause: unknown) {
    this.error = this.sessionFailure(cause);
  }
  get currentID() {
    return this.run?.checkpoint.pass[this.run.checkpoint.position];
  }
  /** Current grouping of the frozen IDs. */
  get stacks(): ReadonlyMap<string, string[]> {
    const groups = this.run?.groups;
    if (!groups) return new Map();
    let index = this.stackIndex.get(groups);
    if (!index) {
      index = new Map(groups.flatMap((group) => group.map((id) => [id, group])));
      this.stackIndex.set(groups, index);
    }
    return index;
  }
  get currentStack() {
    return this.currentID ? this.stacks.get(this.currentID) : undefined;
  }
  /** Check whether the active run needs to regroup under a new gap. */
  async checkSettings() {
    if (!this.run?.stackEligible || this.stackLoading) return;
    try {
      const settings = await getPhotoSettings({ session: this.session });
      if (settings.burst_gap_seconds !== this.run.burstGapSeconds) await this.regroup();
    } catch (cause) {
      this.stackError = this.sessionFailure(cause);
    }
  }
  /** Recompute all frozen IDs and merge into the latest active checkpoint. */
  async regroup() {
    const run = this.run;
    if (!run?.stackEligible || this.stale) return;
    const controller = new AbortController();
    this.stackRead?.abort();
    this.stackRead = controller;
    this.stackLoading = true;
    this.stackError = "";
    try {
      const regrouped = await groupPhotoStacks(
        { asset_ids: run.ids },
        { session: this.session, signal: controller.signal },
      );
      controller.signal.throwIfAborted();
      if (this.run?.checkpoint.runID === run.checkpoint.runID && this.run.ids === run.ids)
        this.run = {
          ...this.run,
          groups: regrouped.groups,
          burstGapSeconds: regrouped.burst_gap_seconds,
        };
    } catch (cause) {
      if (!controller.signal.aborted) this.stackError = this.sessionFailure(cause);
    } finally {
      if (this.stackRead === controller) this.stackLoading = false;
    }
  }
  private passStacks() {
    const pass = this.run?.checkpoint.pass ?? [],
      source = this.run?.groups;
    if (this.passGroups?.pass !== pass || this.passGroups.source !== source)
      this.passGroups = { pass, source, groups: this.groupPass(pass) };
    return this.passGroups.groups;
  }
  private groupPass(pass: string[]) {
    const groups: string[][] = [];
    const byKey = new Map<string, string[]>();
    for (const id of pass) {
      const key = this.stacks.get(id)?.[0] ?? id;
      let group = byKey.get(key);
      if (!group) {
        group = [];
        byKey.set(key, group);
        groups.push(group);
      }
      group.push(id);
    }
    return groups;
  }
  /** A lone frame is its own group, so H/L visits each group's first frame in the pass. */
  moveStack(direction: -1 | 1) {
    if (!this.run || this.busy || this.stale) return;
    const groups = this.passStacks();
    const index = groups.findIndex((group) => group.includes(this.currentID!));
    const next = groups[index + direction];
    if (next) this.choose(next[0]!);
    else if (direction === 1) this.update({ position: this.run.checkpoint.pass.length });
  }
  moveFrame(direction: -1 | 1) {
    if (!this.run || this.busy || this.stale) return;
    if (!this.currentStack) {
      this.move(direction);
      return;
    }
    const group = this.passStacks().find((group) => group.includes(this.currentID!));
    const next = group?.[group.indexOf(this.currentID!) + direction];
    if (next) this.choose(next);
  }
  // Runs in the write chain, so the saved revision matches every update queued before this check.
  async verifyDecision() {
    if (!this.run || this.stale || this.busy || this.stackLoading || this.stackError) return false;
    const expected = this.run.checkpoint;
    let verified = false;
    const check = this.writes.then(async () => {
      if (this.stale) return;
      try {
        const saved = await this.storage.saved();
        if (!saved || !sameCheckpoint(saved, expected))
          throw new Error("Cull progress changed in another tab. Close and resume it.");
        verified = true;
      } catch (cause) {
        this.stale = true;
        this.error = `Progress could not be saved: ${failure(cause)}`;
      }
    });
    this.writes = check;
    await check;
    return verified && !this.stale && !this.busy;
  }
  get complete() {
    return !!this.run && !this.currentID;
  }
  async restore() {
    await this.writes;
    this.error = "";
    try {
      const run = await this.storage.load();
      if (run !== undefined) {
        const checkpoint = run?.checkpoint;
        if (
          !run ||
          !checkpoint ||
          typeof checkpoint.runID !== "string" ||
          !checkpoint.runID.trim() ||
          !Number.isSafeInteger(checkpoint.revision) ||
          checkpoint.revision < 0 ||
          typeof checkpoint.autoAdvance !== "boolean" ||
          ![
            run.ids,
            checkpoint.pass,
            checkpoint.visited,
            checkpoint.skipped,
            checkpoint.unavailable,
          ].every((ids) => Array.isArray(ids) && ids.every((id) => typeof id === "string")) ||
          !scopes.includes(checkpoint.scope) ||
          !Number.isInteger(checkpoint.position) ||
          checkpoint.position < 0 ||
          checkpoint.position > checkpoint.pass.length
        )
          throw new Error("Saved cull progress is invalid. Start a new cull.");
        const captured = new Set(run.ids);
        if (!checkpoint.pass.every((id) => captured.has(id)))
          throw new Error("Saved cull progress is invalid. Start a new cull.");
      }
      this.run = run;
      this.saved = !!run;
      if (this.run)
        for (const kind of ["visited", "skipped", "unavailable"] as const) {
          const list = this.run.checkpoint[kind];
          this.marked.set(list, new Set(list));
        }
      this.stale = false;
      if (this.run) await this.regroup();
      return !!this.run;
    } catch (cause) {
      this.error = failure(cause);
      return false;
    }
  }
  cancel() {
    this.capture?.abort();
    this.stackRead?.abort();
    if (this.flagRead) {
      this.flagRead.abort();
      this.finishFlagRead(this.flagRead);
    }
  }
  async start(
    query: Query,
    confirm: (message: string) => boolean | Promise<boolean> = () => false,
  ) {
    if (this.capturing) return false;
    const controller = (this.capture = new AbortController());
    this.capturing = true;
    this.error = "";
    try {
      await this.writes;
      const previous = await this.storage.saved().catch((cause) => {
        if (cause instanceof CullFormatError) {
          this.error = cause.message;
          return cause.saved;
        }
        throw cause;
      });
      if (previous && !(await confirm("Replace saved cull progress?"))) return false;
      this.progress = "Capturing photos…";
      const page = await createSnapshot(
        this.session,
        query,
        { photos: true, page_size: 250 },
        controller.signal,
      );
      const captured = await captureSnapshotTargets(
        this.session,
        page,
        controller.signal,
        {},
        (loaded, total) => {
          this.progress = `Capturing ${loaded} of ${total} photos…`;
        },
      );
      const ids = captured.members.map((member) => {
        if (!member.asset_id) throw new Error("Photo snapshot membership was incomplete.");
        return member.asset_id;
      });
      const flags = new Map(captured.members.map((member) => [member.asset_id!, member.flag!]));
      const run: CullRun = {
        ids,
        stackEligible: query.sort?.field === "capture_time",
        checkpoint: {
          runID: randomUUID(),
          revision: 0,
          pass: ids.filter((id) => flags.get(id) === ""),
          position: 0,
          visited: [],
          skipped: [],
          unavailable: [],
          scope: "unflagged",
          autoAdvance: true,
        },
      };
      controller.signal.throwIfAborted();
      await this.writes;
      controller.signal.throwIfAborted();
      await this.storage.replace(run, controller.signal, previous);
      this.error = "";
      this.stale = false;
      this.run = run;
      this.saved = true;
      await this.regroup();
      controller.signal.throwIfAborted();
      this.visit();
      return true;
    } catch (cause) {
      if (!controller.signal.aborted) this.apiFailure(cause);
      return false;
    } finally {
      this.capturing = false;
      this.progress = "";
    }
  }
  private async refreshFlags(signal: AbortSignal) {
    const rows = await lookupPhotoRows(
      this.session,
      this.run!.ids,
      signal,
      (loaded, total) => {
        this.progress = `Checking ${loaded} of ${total} photos…`;
      },
      false,
      undefined,
      ({ asset_id, flag }) => ({ asset_id, flag }),
    );
    signal.throwIfAborted();
    return new Map(rows.map((row) => [row.asset_id, row.flag]));
  }
  private matches(scope: CullScope, flags: Map<string, PhotoBrowseRowFlag>) {
    const flag = scope === "picks" ? "pick" : scope === "rejects" ? "reject" : "";
    return this.run!.ids.filter(
      (id) => flags.has(id) && (scope === "all" || flags.get(id) === flag),
    );
  }

  private update(
    patch: Partial<
      Pick<CullCheckpoint, "position" | "autoAdvance" | "visited" | "skipped" | "unavailable">
    >,
    marker?: CullProgress["marker"],
  ) {
    if (!this.run || this.stale || this.changingPass) return;
    const previous = this.run.checkpoint;
    const checkpoint = { ...previous, ...patch, revision: previous.revision + 1 };
    this.run = { ...this.run, checkpoint };
    const progress: CullProgress = {
      runID: previous.runID,
      revision: previous.revision,
      position: patch.position,
      autoAdvance: patch.autoAdvance,
      marker,
    };
    this.writes = this.writes
      .then(async () => {
        if (!this.stale) await this.storage.checkpoint(progress);
      })
      .catch((cause) => {
        this.stale = true;
        this.error = `Progress could not be saved: ${failure(cause)}`;
      });
  }
  // Appends in place: copying a 250,000-photo list on every photo would make culling quadratic.
  private mark(kind: "visited" | "skipped" | "unavailable", added: string[]) {
    if (!this.run || this.stale || this.changingPass) return;
    const list = this.run.checkpoint[kind];
    let ids = this.marked.get(list);
    if (!ids) this.marked.set(list, (ids = new Set(list)));
    const index = list.length;
    for (const id of added) {
      if (ids.has(id)) continue;
      ids.add(id);
      list.push(id);
    }
    if (list.length > index) this.update({ [kind]: list }, { kind, ids: list.slice(index), index });
  }
  private visit() {
    const id = this.currentID;
    if (id) this.mark("visited", [id]);
  }
  choose(id: string) {
    if (this.busy || this.stale) return;
    const position = this.run?.checkpoint.pass.indexOf(id) ?? -1;
    if (position >= 0 && position !== this.run?.checkpoint.position) {
      this.update({ position });
      this.visit();
    }
  }
  move(direction: -1 | 1) {
    if (!this.run || (this.busy && !this.checkingRemainder) || this.stale) return;
    const position = Math.max(
      0,
      Math.min(this.run.checkpoint.pass.length, this.run.checkpoint.position + direction),
    );
    if (position === this.run.checkpoint.position) return;
    // Leaving the completion screen abandons its remainder read.
    if (this.flagRead) {
      this.flagRead.abort();
      this.finishFlagRead(this.flagRead);
    }
    this.update({ position });
    this.visit();
  }
  skip() {
    if (!this.currentID || this.busy || this.stale) return;
    this.mark("skipped", [this.currentID]);
    this.move(1);
  }
  unavailable(id: string) {
    this.mark("unavailable", [id]);
  }
  toggleAutoAdvance() {
    if (this.run) this.update({ autoAdvance: !this.run.checkpoint.autoAdvance });
  }
  private beginFlagRead(remainder = false) {
    this.flagRead?.abort();
    this.busy = true;
    this.checkingRemainder = remainder;
    return (this.flagRead = new AbortController());
  }
  private finishFlagRead(controller: AbortController) {
    if (this.flagRead !== controller) return;
    this.flagRead = undefined;
    this.busy = false;
    this.checkingRemainder = false;
    this.progress = "";
  }
  flagsChanged() {
    this.remainderPass = undefined;
  }
  async checkRemainder() {
    if (!this.run || !this.complete || this.stale) return;
    const pass = this.run.checkpoint.pass;
    if (this.remainderPass === pass) return;
    const controller = this.beginFlagRead(true);
    this.error = "";
    this.remainder = false;
    this.remainderPass = undefined;
    try {
      const flags = await this.refreshFlags(controller.signal);
      controller.signal.throwIfAborted();
      this.mark(
        "unavailable",
        this.run.ids.filter((id) => !flags.has(id)),
      );
      this.remainder = this.matches("unflagged", flags).length > 0;
      this.remainderPass = pass;
    } catch (cause) {
      if (!controller.signal.aborted) this.apiFailure(cause);
    } finally {
      this.finishFlagRead(controller);
    }
  }
  async nextPass(
    followUp = false,
    confirm: (message: string) => boolean | Promise<boolean> = () => false,
  ) {
    if (!this.run || this.busy || this.stale) return;
    const controller = this.beginFlagRead();
    this.changingPass = true;
    let changed = false;
    this.error = "";
    try {
      if (
        !followUp &&
        !this.complete &&
        !(await confirm("Change scope and reset this pass's progress?"))
      )
        return;
      await this.writes;
      controller.signal.throwIfAborted();
      if (this.stale) return;
      const previous = this.run.checkpoint;
      const ids = this.run.ids;
      const flags = await this.refreshFlags(controller.signal);
      controller.signal.throwIfAborted();
      const scope = followUp
        ? "unflagged"
        : scopes[(scopes.indexOf(this.run.checkpoint.scope) + 1) % scopes.length]!;
      const pass = this.matches(scope, flags);
      if (followUp && !pass.length) {
        this.changingPass = false;
        this.mark(
          "unavailable",
          ids.filter((id) => !flags.has(id)),
        );
        this.remainder = false;
        return;
      }
      const checkpoint: CullCheckpoint = {
        ...previous,
        revision: previous.revision + 1,
        scope,
        pass,
        position: 0,
        visited: [],
        skipped: [],
        unavailable: ids.filter((id) => !flags.has(id)),
      };
      try {
        await this.storage.checkpoint({
          runID: previous.runID,
          revision: previous.revision,
          pass: checkpoint,
        });
      } catch (cause) {
        this.stale = true;
        throw cause;
      }
      this.run = { ...this.run, checkpoint };
      this.remainder = false;
      changed = true;
    } catch (cause) {
      if (!controller.signal.aborted) this.apiFailure(cause);
    } finally {
      this.changingPass = false;
      this.finishFlagRead(controller);
    }
    if (changed) this.visit();
  }
  flush() {
    return this.writes;
  }
}
