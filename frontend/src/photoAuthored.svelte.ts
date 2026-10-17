import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import type { HiddenAccess } from "./hiddenAccess.svelte.js";

export class PhotoUndoState {
  receiptID = $state("");
  summary = $state("");
}

const pendingXMP = (target: api.PhotoPreparedTarget) => target.xmp?.state === "pending";
export const xmpPollDelay = 5_000;
const xmpPolls = 12;

export class PhotoAuthoredEditor {
  private xmpRecheck?: ReturnType<typeof setTimeout>;
  private xmpPolls = 0;
  private refreshedXMPReceipts = new Set<string>();
  // Poll while a shown original is Pending; a busy or loading editor retries
  // later without spending an attempt, and the last attempt asks for a manual refresh.
  private recheckPendingXMP() {
    clearTimeout(this.xmpRecheck);
    this.xmpStalled = false;
    if (this.controller.signal.aborted || !this.targets.some(pendingXMP)) {
      this.xmpPolls = 0;
      return;
    }
    if (this.xmpPolls >= xmpPolls) {
      this.xmpStalled = true;
      return;
    }
    this.xmpRecheck = setTimeout(() => void this.refreshXMP(true), xmpPollDelay);
  }
  onXMPChanged?: (receipt?: api.PhotoAuthoredReceipt, tags?: api.Tag[]) => Promise<void>;
  xmpError = $state("");
  xmpStalled = $state(false);
  async refreshXMP(poll = false) {
    if (!poll) this.xmpPolls = 0;
    if (this.busy || this.loading || this.concealed) {
      this.recheckPendingXMP();
      return;
    }
    if (poll) this.xmpPolls++;
    this.xmpError = "";
    const write = this.beginWrite();
    try {
      await this.refreshPreparedXMP();
    } catch (cause) {
      if (!this.controller.signal.aborted) this.xmpError = this.failure(cause);
    } finally {
      this.endWrite(write);
    }
  }
  private async refreshPreparedXMP(rememberImport = false) {
    const snapshot = () =>
      JSON.stringify(this.targets.map((target) => [target.xmp?.state, target.asset_revision]));
    const previous = snapshot();
    const conflict = this.conflict,
      checks = this.draftRevisions();
    await this.prepareMembers();
    this.conflict ||= conflict || checks !== this.draftRevisions();
    this.recheckPendingXMP();
    if (this.error) throw new Error(this.error);
    let notified = false;
    for (const target of this.targets) {
      const receipt = target.xmp?.receipt;
      if (!receipt || this.refreshedXMPReceipts.has(receipt.receipt_id)) continue;
      if (rememberImport) {
        this.receiptID = receipt.receipt_id;
        this.undoState.summary = "Sidecar metadata imported";
      }
      const tags = await this.refreshKeywords(receipt);
      await this.onXMPChanged?.(receipt, tags);
      notified = true;
      this.refreshedXMPReceipts.add(receipt.receipt_id);
    }
    if (!notified && previous !== snapshot()) await this.onXMPChanged?.();
  }
  async undoSidecarImport(fileID: string) {
    const receiptID = this.targets.find((target) => target.file_id === fileID)?.xmp?.receipt
      ?.receipt_id;
    if (!receiptID || this.busy || this.loading || this.dirty || this.concealed) return;
    await this.undo(async (receipt, tags) => {
      await this.onXMPChanged?.(receipt, tags);
    }, receiptID);
  }

  async resolveXMP(fileID: string, choice: api.PhotoXMPResolveRequestChoice) {
    if (this.busy || this.loading || this.concealed || this.dirty) return;
    const expected = this.targets.find((target) => target.file_id === fileID)?.xmp;
    if (!expected || expected.state !== "conflict") return;
    const write = this.beginWrite();
    this.xmpError = "";
    try {
      await this.request(() =>
        api.resolvePhotoXMP(fileID, { conflict_id: expected.conflict_id!, choice }, this.options()),
      );
      if (this.controller.signal.aborted) return;
      await this.refreshPreparedXMP(true);
    } catch (cause) {
      if (!this.controller.signal.aborted) {
        this.xmpError =
          cause instanceof APIError && [404, 409, 412].includes(cause.status)
            ? "The photo or sidecar changed. Refresh XMP and choose again."
            : this.failure(cause);
      }
    } finally {
      this.endWrite(write);
    }
  }
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
    const assigned = new Set(this.targets.flatMap((target) => target.tags)),
      supplied = new Set(tags.map((tag) => tag.id));
    this.tags = [
      ...tags,
      ...this.tags.filter((tag) => assigned.has(tag.id) && !supplied.has(tag.id)),
    ];
    void this.resolveAssignedTags();
  }
  private pendingTags = new Map<string, Promise<void>>();
  private async resolveAssignedTags() {
    const known = new Set(this.tags.map((tag) => tag.id));
    const missing = [...new Set(this.targets.flatMap((target) => target.tags))].filter(
      (id) => !known.has(id) && !this.pendingTags.has(id),
    );
    for (const id of missing) {
      const request = this.request(() => api.getTag(id, this.options()))
        .then((tag) => {
          if (
            this.pendingTags.get(id) === request &&
            !this.controller.signal.aborted &&
            this.targets.some((target) => target.tags.includes(id)) &&
            !this.tags.some((value) => value.id === id)
          )
            this.tags = [...this.tags, tag];
        })
        .catch((cause) => {
          if (this.pendingTags.get(id) === request && !this.controller.signal.aborted)
            this.tagsError = this.failure(cause);
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
          const tag = await this.request(() => api.getTag(id, this.options()));
          if (this.controller.signal.aborted) return;
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
    return refreshed.filter((tag): tag is api.Tag => !!tag);
  }
  private controller = new AbortController();
  private read = new AbortController();
  private scope?: api.PhotoAuthoredPrepareRequest;
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
    if (write === this.writes) this.busy = false;
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
    this.xmpError = "";
    this.refreshedXMPReceipts.clear();
    clearTimeout(this.xmpRecheck);
    this.xmpPolls = 0;
    this.xmpStalled = false;
    this.read.abort();
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
    this.validating = false;
    this.concealed = false;
  }
  conceal() {
    this.read.abort();
    this.loading = false;
    this.concealed = true;
    this.validating = true;
  }
  revalidate(eligible: Set<string>) {
    const targets = this.targets.filter((target) => eligible.has(target.asset_id));
    if (targets.length !== this.targets.length) {
      this.fileID = "";
      if (this.dirty || this.busy) {
        this.membershipChanged = false;
        this.conflict = true;
      }
    }
    this.targets = targets;
    if (this.asset && !eligible.has(this.asset.id)) this.asset = undefined;
    this.validating = false;
    this.concealed = false;
  }
  async prepare(scope: api.PhotoAuthoredPrepareRequest, reload = false) {
    this.read.abort();
    const controller = (this.read = new AbortController());
    const signal = AbortSignal.any([controller.signal, this.controller.signal]);
    this.loading = true;
    this.error = "";
    const previous = reload ? new Set(this.targets.map((target) => target.file_id)) : undefined;
    try {
      const prepared = await this.request(() =>
        api.preparePhotoAuthored(scope, this.options(signal)),
      );
      if (!Array.isArray(prepared.assets) || !Array.isArray(prepared.targets))
        throw new Error("Metadata response was incomplete. Reload metadata.");
      if (signal.aborted) return;
      if (
        previous &&
        (prepared.targets.length !== previous.size ||
          [...previous].some((id) => !prepared.targets.some((target) => target.file_id === id)))
      ) {
        this.membershipChanged = true;
        if (!prepared.targets.some((target) => target.file_id === this.fileID)) this.fileID = "";
      }
      this.targets = previous
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
      this.scope = scope;
      this.conflict = this.membershipChanged;
      void this.resolveAssignedTags();
      if (!this.tags.length && !this.tagsLoading && !this.tagsError)
        void this.loadTags?.().catch((cause) => {
          if (!signal.aborted) this.tagsError = this.failure(cause);
        });
    } catch (cause) {
      if (!signal.aborted) this.error = this.failure(cause);
    } finally {
      if (!signal.aborted) {
        this.loading = false;
        if (!reload) this.recheckPendingXMP();
      }
    }
  }
  async reload() {
    if (this.busy || this.loading || !this.targets.length) return;
    await this.prepareMembers();
  }
  private prepareMembers() {
    return this.prepare(
      {
        asset_ids: [...new Set(this.targets.map((target) => target.asset_id))],
        hidden: this.scope?.hidden,
        skip_unavailable: true,
      },
      true,
    );
  }
  private draftRevisions() {
    if (!this.dirty) return "";
    return JSON.stringify(
      this.targets.map(({ file_id, asset_revision, revision, node_revision }) => [
        file_id,
        asset_revision,
        revision,
        node_revision,
      ]),
    );
  }
  reviewMembers() {
    this.membershipChanged = false;
    this.conflict = false;
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
        xmp: target.xmp
          ? { ...target.xmp, state: "pending" as const, reason: undefined }
          : undefined,
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
    if (this.busy || this.loading || this.concealed) {
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
      if (this.files.some((target) => target.xmp)) await this.refreshXMP();
    } catch (cause) {
      if (!this.controller.signal.aborted) {
        this.error = this.failure(cause);
        if (cause instanceof APIError && cause.errors.length) this.error = cause.errors.join("; ");
        this.conflict = cause instanceof APIError && [404, 409, 412].includes(cause.status);
      }
    } finally {
      this.endWrite(write);
    }
  }
  async undo(
    refresh: (receipt: api.PhotoAuthoredReceipt, tags: api.Tag[]) => Promise<void>,
    receiptID = this.receiptID,
  ) {
    if (this.busy || this.loading || this.concealed) {
      this.unavailable();
      return;
    }
    if (!receiptID || this.controller.signal.aborted) return;
    const write = this.beginWrite();
    this.error = "";
    try {
      const receipt = await this.request(() => api.undoPhotoAuthored(receiptID, this.options()));
      this.accept(receipt);
      if (this.receiptID === receiptID) {
        this.receiptID = "";
        this.undoState.summary = "";
      }
      this.notice = receipt.receipt_id ? "Edit undone" : "No changes to undo";
      if (!receipt.receipt_id) return;
      const tags = await this.refreshKeywords(receipt);
      this.endWrite(write);
      await refresh(receipt, tags);
      if (this.files.some((target) => target.xmp)) await this.refreshXMP();
    } catch (cause) {
      if (!this.controller.signal.aborted) this.error = this.failure(cause);
    } finally {
      this.endWrite(write);
    }
  }
  dispose() {
    clearTimeout(this.xmpRecheck);
    this.controller.abort();
    this.read.abort();
  }
}
