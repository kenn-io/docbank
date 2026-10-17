import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import type { HiddenAccess } from "./hiddenAccess.svelte.js";
import type { PhotoEventBatch } from "./photoEvents.js";

const prepareTimeout = 60_000;

export function mergePhotoTags(current: api.Tag[], incoming: api.Tag[]) {
  const merged = new Map(current.map((tag) => [tag.id, tag]));
  for (const tag of incoming)
    if (!merged.has(tag.id) || (merged.get(tag.id)!.revision ?? 0) <= (tag.revision ?? 0))
      merged.set(tag.id, tag);
  return [...merged.values()];
}

export class PhotoUndoState {
  receiptID = $state("");
  summary = $state("");
}

export class PhotoAuthoredEditor {
  asset = $state<api.PhotoAsset>();
  targets = $state<api.PhotoPreparedTarget[]>([]);
  tags = $state<api.Tag[]>([]);
  tagsError = $state("");
  tagsLoading = $state(false);
  tagsTotal = $state(0);
  fileID = $state("");
  patch = $state<api.PhotoAuthoredPatch>({});
  rotationDelta = $state<api.PhotoAuthoredEditRequest["rotation_delta"]>();
  keywords = $state<api.PhotoKeywordEdit[]>([]);
  loading = $state(false);
  comparing = $state(false);
  validating = $state(false);
  concealed = $state(false);
  busy = $state(false);
  conflict = $state(false);
  membershipChanged = $state(false);
  error = $state("");
  notice = $state("");
  noticeTone = $state<"success" | "warning">("success");
  get receiptID() {
    return this.undoState.receiptID;
  }
  set receiptID(value: string) {
    this.undoState.receiptID = value;
  }
  inspectedID = $state("");
  loadTags?: () => Promise<void>;
  loadMoreTags?: () => Promise<void>;
  catalog(tags: api.Tag[]) {
    for (const tag of tags) if (this.deletedTags.has(tag.id)) this.onTagDeleted?.(tag.id);
    tags = tags.filter((tag) => !this.deletedTags.has(tag.id));
    const assigned = new Set([
      ...this.targets.flatMap((target) => target.tags),
      ...this.keywords.map((keyword) => keyword.tag_id),
    ]);
    this.tags = mergePhotoTags(
      tags,
      this.tags.filter((tag) => assigned.has(tag.id)),
    );
    void this.resolveAssignedTags();
  }
  private definitionRefresh = 0;
  async refreshAssignedTags() {
    const generation = ++this.definitionRefresh;
    const ids = [
      ...new Set([
        ...this.targets.flatMap((target) => target.tags),
        ...this.keywords.map((keyword) => keyword.tag_id),
      ]),
    ];
    const results = await Promise.allSettled(
      ids.map(async (id) => {
        try {
          return await this.request(() => api.getTag(id, this.boundedOptions()));
        } catch (cause) {
          if (!(cause instanceof APIError && cause.status === 404)) throw cause;
          if (generation === this.definitionRefresh && !this.controller.signal.aborted)
            this.deleteTag(id);
        }
      }),
    );
    if (generation !== this.definitionRefresh || this.controller.signal.aborted) return [];
    const failed = results.find((result) => result.status === "rejected");
    if (failed) {
      this.tagsError =
        failed.reason instanceof Error ? failed.reason.message : String(failed.reason);
      throw failed.reason;
    }
    const retained = results.flatMap((result) => {
      if (result.status !== "fulfilled" || !result.value) return [];
      const tag = result.value;
      return ids.includes(tag.id) && !this.deletedTags.has(tag.id) ? [tag] : [];
    });
    this.tags = mergePhotoTags(this.tags, retained);
    this.tagsError = "";
    return this.tags.filter((tag) => ids.includes(tag.id) && !this.deletedTags.has(tag.id));
  }
  onTagDeleted?: (id: string) => void;
  private deletedTags = new Set<string>();
  private deleteTag(id: string) {
    const name = this.tags.find((tag) => tag.id === id)?.name;
    if (this.keywords.some((keyword) => keyword.tag_id === id)) {
      this.keywords = this.keywords.filter((keyword) => keyword.tag_id !== id);
      this.notice = `${name ? `“${name}”` : "A keyword"} was deleted. Its staged edit was removed.`;
      this.noticeTone = "warning";
    }
    this.deletedTags.add(id);
    this.pendingTags.delete(id);
    this.tags = this.tags.filter((tag) => tag.id !== id);
    this.onTagDeleted?.(id);
  }
  private pendingTags = new Map<string, Promise<void>>();
  private async resolveAssignedTags() {
    const known = new Set(this.tags.map((tag) => tag.id));
    const missing = [...new Set(this.targets.flatMap((target) => target.tags))].filter(
      (id) => !known.has(id) && !this.deletedTags.has(id) && !this.pendingTags.has(id),
    );
    for (const id of missing) {
      const request = this.request(() => api.getTag(id, this.boundedOptions()))
        .then((tag) => {
          if (
            this.pendingTags.get(id) === request &&
            !this.controller.signal.aborted &&
            !this.deletedTags.has(id) &&
            this.targets.some((target) => target.tags.includes(id)) &&
            !this.tags.some((value) => value.id === id)
          )
            this.tags = [...this.tags, tag];
        })
        .catch((cause) => {
          if (this.pendingTags.get(id) === request && !this.controller.signal.aborted) {
            if (cause instanceof APIError && cause.status === 404) this.deleteTag(id);
            else this.tagsError = this.failure(cause);
          }
        })
        .finally(() => {
          if (this.pendingTags.get(id) === request) this.pendingTags.delete(id);
        });
      this.pendingTags.set(id, request);
    }
    await Promise.all(this.pendingTags.values());
  }
  private async refreshKeywords(receipt: api.PhotoAuthoredReceipt) {
    const changed = new Set(receipt.keywords?.map((change) => change.tag_id));
    if (!changed.size) return [];
    for (const id of changed) this.pendingTags.delete(id);
    const refreshed = await Promise.all(
      [...changed].map(async (id) => {
        try {
          const tag = await this.request(() => api.getTag(id, this.boundedOptions()));
          if (this.controller.signal.aborted || this.deletedTags.has(id)) return;
          if (this.tags.some((value) => value.id === id))
            this.tags = this.tags.map((value) => (value.id === id ? tag : value));
          else if (this.targets.some((target) => target.tags.includes(id)))
            this.tags = [...this.tags, tag];
          return tag;
        } catch (cause) {
          if (!this.controller.signal.aborted) this.tagsError = this.failure(cause);
        }
      }),
    );
    return refreshed.filter((tag): tag is api.Tag => !!tag && !this.deletedTags.has(tag.id));
  }
  private controller = new AbortController();
  private read = new AbortController();
  private scope?: api.PhotoAuthoredPrepareRequest;
  private preparing?: {
    scope: api.PhotoAuthoredPrepareRequest;
    done: Promise<void>;
    finish: () => void;
    followup?: Promise<void>;
    refreshedScope?: api.PhotoAuthoredPrepareRequest;
  };
  constructor(
    private session: string,
    private onfailure: (cause: unknown) => void,
    readonly undoState = new PhotoUndoState(),
    private readonly hiddenAccess?: HiddenAccess,
  ) {}
  private request<T>(request: () => Promise<T>) {
    return this.hiddenAccess ? this.hiddenAccess.run(request) : request();
  }
  private options(signal = this.controller.signal) {
    return { session: this.session, signal };
  }
  private boundedOptions() {
    return this.options(
      AbortSignal.any([this.controller.signal, AbortSignal.timeout(prepareTimeout)]),
    );
  }
  private failure(cause: unknown) {
    this.onfailure(cause);
    return cause instanceof Error ? cause.message : String(cause);
  }
  // A write releases busy before its list refresh, so a later write may own busy by the time this one settles.
  private writes = 0;
  private beginWrite() {
    this.busy = true;
    return ++this.writes;
  }
  private endWrite(write: number) {
    if (write !== this.writes) return;
    this.busy = false;
    void this.reconcile();
  }
  // Another writer changed a target. Writes reconcile once they settle; unsaved drafts keep the conflict.
  // A change stays pending until a read that started after it succeeds.
  private remoteChange = false;
  private membershipConflict = false;
  private remoteChanges = 0;
  private failedComparison?: number;
  private reconciling?: { read: AbortController; done: Promise<void>; change: number };
  get preparingQuery() {
    return this.preparing?.scope.query;
  }
  affectedBy(changes: PhotoEventBatch) {
    const pending = this.preparing?.scope;
    return (
      ((!!pending || !!this.targets.length) && changes.refetch) ||
      !!pending?.asset_ids?.some((id) => changes.assets.has(id)) ||
      (!!pending?.query && !!changes.assets.size) ||
      this.targets.some((target) => changes.assets.has(target.asset_id))
    );
  }
  changedElsewhere(changes?: PhotoEventBatch) {
    if (!changes || this.targets.some((target) => changes.assets.has(target.asset_id)))
      this.conflict = true;
    this.remoteChange = true;
    this.remoteChanges++;
  }
  async reconcile(scope?: api.PhotoAuthoredPrepareRequest) {
    if (!this.remoteChange || this.busy || this.failedComparison === this.remoteChanges) return;
    const preparing = this.preparing;
    if (preparing) {
      // Only a successful list refresh supplies the current scope for this one follow-up.
      if (preparing.followup) return preparing.followup;
      if (scope) preparing.refreshedScope = scope;
      if (!preparing.refreshedScope) return;
      await preparing.done;
      if (
        this.preparing !== preparing ||
        this.controller.signal.aborted ||
        this.concealed ||
        this.validating
      )
        return;
      if (!preparing.followup) {
        preparing.followup = this.prepare(preparing.refreshedScope, false, this.dirty, true);
      }
      await preparing.followup;
      if (this.preparing === preparing) {
        this.preparing = undefined;
        if (this.read.signal.aborted) {
          if (!this.concealed && !this.validating) await this.reconcile();
          return;
        }
        if (this.remoteChange) {
          this.conflict = true;
          this.failedComparison = this.remoteChanges;
        }
      }
      return;
    }
    if (!this.targets.length) {
      this.remoteChange = false;
      this.conflict = this.dirty || this.membershipChanged;
      return;
    }
    // Share a live reconcile; a failed one waits for Reload metadata or the next change.
    if (!this.reconciling || this.reconciling.read.signal.aborted) {
      const change = this.remoteChanges;
      const done = this.prepare(this.reloadScope(), true, true);
      this.reconciling = { read: this.read, done, change };
    }
    const reconciling = this.reconciling;
    await reconciling.done;
    if (this.reconciling === reconciling) {
      this.reconciling = undefined;
      if (this.remoteChanges !== reconciling.change) await this.reconcile();
    }
  }
  canChangeTarget() {
    if (this.loading) {
      this.unavailable();
      return false;
    }
    if (!this.dirty && !this.busy) return true;
    this.unavailable(
      this.busy
        ? "An edit is in progress. Wait for it to finish."
        : "Apply or clear your metadata edits before changing photos.",
    );
    return false;
  }
  reset() {
    this.read.abort();
    this.preparing = undefined;
    this.remoteChange = false;
    this.failedComparison = undefined;
    this.membershipConflict = false;
    this.targets = [];
    this.asset = undefined;
    this.fileID = "";
    this.patch = {};
    this.rotationDelta = undefined;
    this.keywords = [];
    this.scope = undefined;
    this.error = "";
    this.conflict = false;
    this.membershipChanged = false;
    this.loading = false;
    this.comparing = false;
    this.validating = false;
    this.concealed = false;
  }
  conceal() {
    this.read.abort();
    this.loading = false;
    this.comparing = false;
    this.concealed = true;
    this.validating = true;
  }
  revalidate(eligible: Set<string>, scope?: api.PhotoAuthoredPrepareRequest) {
    const targets = this.targets.filter((target) => eligible.has(target.asset_id));
    if (targets.length !== this.targets.length) {
      this.fileID = "";
      if (this.dirty || this.busy) {
        this.membershipConflict = true;
        this.membershipChanged = false;
        this.conflict = true;
      }
    }
    this.targets = targets;
    if (this.asset && !eligible.has(this.asset.id)) this.asset = undefined;
    this.validating = false;
    this.concealed = false;
    // A list refresh conceals the editor and aborts its reads, including a pending reconcile.
    void this.reconcile(scope);
  }
  async prepare(
    scope: api.PhotoAuthoredPrepareRequest,
    reload = false,
    compare = false,
    followup = false,
  ) {
    this.read.abort();
    if (!followup) {
      this.preparing = undefined;
      if (!reload && !compare) {
        let finish!: () => void;
        const done = new Promise<void>((resolve) => (finish = resolve));
        this.scope = scope;
        this.preparing = { scope, done, finish };
      }
    }
    const preparing = this.preparing;
    const controller = (this.read = new AbortController());
    const signal = AbortSignal.any([controller.signal, this.controller.signal]);
    // A stalled read would hold a live refresh and leave Reload metadata unavailable.
    const deadline = AbortSignal.timeout(prepareTimeout);
    this.loading = true;
    this.comparing = compare;
    this.error = "";
    const previous =
      reload && (this.targets.length || this.scope?.query)
        ? new Set(this.targets.map((target) => target.file_id))
        : undefined;
    const change = this.remoteChanges;
    try {
      const prepared =
        scope.asset_ids?.length === 0
          ? { assets: [], targets: [] }
          : await this.request(() =>
              api.preparePhotoAuthored(scope, this.options(AbortSignal.any([signal, deadline]))),
            );
      if (!Array.isArray(prepared.assets) || !Array.isArray(prepared.targets))
        throw new Error("Metadata response was incomplete. Reload metadata.");
      if (signal.aborted) return;
      if (compare) {
        const same =
          prepared.targets.length === this.targets.length &&
          this.targets.every((base) =>
            prepared.targets.some(
              (target) =>
                target.file_id === base.file_id &&
                target.asset_id === base.asset_id &&
                target.node_id === base.node_id &&
                target.asset_revision === base.asset_revision &&
                target.revision === base.revision &&
                target.node_revision === base.node_revision,
            ),
          );
        if (change === this.remoteChanges) this.remoteChange = false;
        this.conflict =
          this.membershipChanged || this.membershipConflict || !same || this.remoteChange;
        if (this.dirty) return;
      }
      if (
        previous &&
        (prepared.targets.length !== previous.size ||
          [...previous].some((id) => !prepared.targets.some((target) => target.file_id === id)))
      ) {
        this.membershipChanged = true;
        if (!prepared.targets.some((target) => target.file_id === this.fileID)) this.fileID = "";
      }
      this.targets =
        previous && !this.scope?.query
          ? prepared.targets.filter((target) => previous.has(target.file_id))
          : prepared.targets;
      const asset = prepared.assets.length === 1 ? prepared.assets[0] : undefined;
      this.asset = asset
        ? {
            ...asset,
            files: asset.files.filter((file) =>
              this.targets.some((target) => target.file_id === file.id),
            ),
          }
        : undefined;
      this.membershipConflict = false;
      if (change === this.remoteChanges) this.remoteChange = false;
      this.conflict = this.membershipChanged || this.remoteChange;
      void this.resolveAssignedTags();
      if (!this.tags.length && !this.tagsLoading && !this.tagsError)
        void this.loadTags?.().catch((cause) => {
          if (!signal.aborted) this.tagsError = this.failure(cause);
        });
    } catch (cause) {
      if (!signal.aborted) {
        if (compare || reload || followup || scope.query || scope.asset_ids?.length) {
          this.conflict = true;
          this.failedComparison = change;
        }
        this.error = deadline.aborted
          ? "Metadata timed out. Reload metadata."
          : this.failure(cause);
      }
    } finally {
      if (!followup && preparing) {
        preparing.finish();
        if (this.preparing === preparing && !this.remoteChange) this.preparing = undefined;
      }
      if (!signal.aborted) {
        this.loading = false;
        this.comparing = false;
      }
    }
  }
  get canReload() {
    return !!this.scope?.query || !!this.scope?.asset_ids?.length || !!this.targets.length;
  }
  async reload() {
    if (this.busy || this.loading || !this.canReload) return;
    await this.prepare(
      this.scope?.query || !this.targets.length ? this.scope! : this.reloadScope(),
      true,
    );
  }
  private reloadScope(): api.PhotoAuthoredPrepareRequest {
    return {
      asset_ids: [...new Set(this.targets.map((target) => target.asset_id))],
      hidden: this.scope?.hidden,
      skip_unavailable: true,
    };
  }
  reviewMembers() {
    this.membershipChanged = false;
    this.conflict = this.remoteChange || this.membershipConflict;
  }
  private selectedFiles = $derived(
    this.targets.filter((target) => !this.fileID || target.file_id === this.fileID),
  );
  private keywordCounts = $derived.by(() => {
    const counts = new Map<string, number>();
    for (const file of this.selectedFiles)
      for (const tag of file.tags) counts.set(tag, (counts.get(tag) ?? 0) + 1);
    return counts;
  });
  get files() {
    return this.selectedFiles;
  }
  get count() {
    return new Set(this.targets.map((target) => target.asset_id)).size;
  }
  get dirty() {
    return (
      Object.keys(this.patch).length > 0 ||
      this.rotationDelta !== undefined ||
      this.keywords.length > 0
    );
  }
  value(field: keyof api.PhotoAuthoredPatch) {
    if (this.patch[field] !== undefined) return this.patch[field];
    const files = this.files;
    if (!files.length || files.some((file) => file.values[field] !== files[0]!.values[field]))
      return undefined;
    return files[0]!.values[field];
  }
  stage(patch: api.PhotoAuthoredPatch, keywords: api.PhotoKeywordEdit[] = []) {
    this.patch = { ...this.patch, ...patch };
    if (patch.rotation !== undefined) this.rotationDelta = undefined;
    for (const keyword of keywords)
      this.keywords = [
        ...this.keywords.filter((value) => value.tag_id !== keyword.tag_id),
        keyword,
      ];
  }
  clearDraft() {
    this.patch = {};
    this.rotationDelta = undefined;
    this.keywords = [];
    if (!this.targets.length) {
      this.conflict = false;
      this.membershipChanged = false;
    }
  }
  member(fileID: string) {
    if (!this.dirty) this.fileID = fileID;
  }
  keywordState(tagID: string) {
    const staged = this.keywords.find((keyword) => keyword.tag_id === tagID);
    if (staged) return staged.assign ? "assigned" : "";
    const count = this.keywordCounts.get(tagID) ?? 0;
    return count === 0 ? "" : count === this.files.length ? "assigned" : "mixed";
  }
  unavailable(
    message = this.concealed && !this.validating
      ? "Retry the photo refresh before editing metadata."
      : this.loading || this.validating
        ? "Metadata is loading. Try again when it finishes."
        : "An edit is in progress. Try the shortcut again when it finishes.",
  ) {
    this.noticeTone = "warning";
    this.notice = message;
  }
  private accept(receipt: api.PhotoAuthoredReceipt) {
    this.noticeTone = "success";
    this.targets = this.targets.map((target) => {
      const after = receipt.after.find((file) => file.file_id === target.file_id);
      if (!after) return target;
      const tags = new Set(target.tags);
      for (const change of receipt.keywords ?? [])
        if (change.node_id === target.node_id) {
          if (change.after) tags.add(change.tag_id);
          else tags.delete(change.tag_id);
        }
      const nodeRevision =
        receipt.keyword_nodes?.find((node) => node.node_id === target.node_id)?.after_revision ??
        target.node_revision + (after.revision - target.revision);
      return {
        ...target,
        revision: after.revision,
        node_revision: nodeRevision,
        values: after.values,
        tags: [...tags],
      };
    });
  }
  async edit(
    scope: api.PhotoAlbumMembersRequest,
    patch: api.PhotoAuthoredPatch,
    hidden: boolean,
    refresh: (receipt: api.PhotoAuthoredReceipt, tags: api.Tag[]) => Promise<void>,
    keywords?: api.PhotoKeywordEdit[],
    rotationDelta?: api.PhotoAuthoredEditRequest["rotation_delta"],
  ) {
    if (this.conflict) {
      this.unavailable("Reload metadata before editing.");
      return;
    }
    if (this.busy || this.loading || this.validating || this.concealed) {
      this.unavailable();
      return;
    }
    if (this.controller.signal.aborted) return;
    if (!this.targets.length) await this.prepare({ ...scope, hidden });
    if (!this.targets.length || this.conflict) return;
    this.stage(patch, keywords);
    if (rotationDelta !== undefined) {
      const current = { ...this.patch };
      delete current.rotation;
      this.patch = current;
      this.rotationDelta = rotationDelta;
    }
    if (!this.dirty) return;
    const write = this.beginWrite();
    this.error = "";
    this.notice = "";
    try {
      const edits = [
        ...Object.keys(this.patch).map((field) => (field === "label" ? "color label" : field)),
        ...(this.rotationDelta !== undefined ? ["rotation"] : []),
        ...(this.keywords.length ? ["keywords"] : []),
      ];
      const receipt = await this.request(() =>
        api.editPhotoAuthored(
          {
            targets: this.files.map(
              ({ asset_id, asset_revision, file_id, revision, node_revision }) => ({
                asset_id,
                asset_revision,
                file_id,
                revision,
                node_revision,
              }),
            ),
            patch: this.patch,
            rotation_delta: this.rotationDelta,
            keywords: this.keywords,
          },
          this.options(),
        ),
      );
      this.accept(receipt);
      this.patch = {};
      this.rotationDelta = undefined;
      this.keywords = [];
      if (!receipt.receipt_id) {
        this.notice = "No changes to apply";
        return;
      }
      this.receiptID = receipt.receipt_id;
      this.undoState.summary = `${receipt.after.length} file${receipt.after.length === 1 ? "" : "s"} updated: ${edits.join(", ")}`;
      const tags = await this.refreshKeywords(receipt);
      this.endWrite(write);
      await refresh(receipt, tags);
    } catch (cause) {
      if (!this.controller.signal.aborted) {
        this.error = this.failure(cause);
        if (cause instanceof APIError && cause.errors.length) this.error = cause.errors.join("; ");
        this.conflict ||= cause instanceof APIError && [404, 409, 412].includes(cause.status);
      }
    } finally {
      this.endWrite(write);
    }
  }
  async undo(refresh: (receipt: api.PhotoAuthoredReceipt, tags: api.Tag[]) => Promise<void>) {
    if (this.busy || this.loading || this.validating || this.concealed) {
      this.unavailable();
      return;
    }
    if (!this.receiptID || this.controller.signal.aborted) return;
    const write = this.beginWrite();
    this.error = "";
    try {
      const receipt = await this.request(() =>
        api.undoPhotoAuthored(this.receiptID, this.options()),
      );
      this.accept(receipt);
      this.receiptID = "";
      this.undoState.summary = "";
      this.notice = receipt.receipt_id ? "Edit undone" : "No changes to undo";
      if (!receipt.receipt_id) return;
      const tags = await this.refreshKeywords(receipt);
      this.endWrite(write);
      await refresh(receipt, tags);
    } catch (cause) {
      if (!this.controller.signal.aborted) this.error = this.failure(cause);
    } finally {
      this.endWrite(write);
    }
  }
  dispose() {
    this.controller.abort();
    this.read.abort();
  }
}
