<script lang="ts">
  import UploadChannelNotice from "./UploadChannelNotice.svelte";
  import type { UploadConnectionState } from "./uploadReconnect.js";
  import { reportAction, reportFailure } from "./action-events.js";
  import { onDestroy } from "svelte";
  import { Button, TextInput } from "@kenn-io/kit-ui";
  import DrawerShell from "./DrawerShell.svelte";
  import "./toolForm.css";
  import { APIError } from "./api-transport.js";
  import {
    UploadChannelError,
    photoImportDeclaration,
    type PhotoImportTransport,
    type PhotoGroupReceipt,
    type TransferProgress,
  } from "./upload.js";

  import { getBrowserPhotoImport } from "./generated/docbank.js";
  import LocalFilePicker from "./LocalFilePicker.svelte";
  import { localFilePath, type PickerKind } from "./localFiles.js";
  import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";

  import { randomUUID } from "./crypto.js";
  import {
    photoImportManifest,
    photoImportStorage,
    photoImportStorageUnavailable,
    settledPhotoImports,
    type PhotoImportCheckpoint,
  } from "./photoImportStorage.js";

  let {
    channel,
    vaultID,
    session,
    disabledReason = "",
    connectionState = "fatal",
    onreconnect = () => {},
    onclose,
    oncomplete,
    onauthfailure,
  }: {
    channel: PhotoImportTransport;
    vaultID: string;
    session: string;
    disabledReason?: string;
    connectionState?: UploadConnectionState;
    onreconnect?: () => void;
    onclose: () => void;
    oncomplete: () => void | Promise<void>;
    onauthfailure: (cause: unknown) => void;
  } = $props();
  type Item = {
    group: number;
    files: File[];
    hashes: string[];
    state:
      | "ready"
      | "hashing"
      | "uploading"
      | "unconfirmed"
      | "failed"
      | PhotoGroupReceipt["status"];
    progress: TransferProgress;
    error?: string;
  };
  type Recovery = {
    sourceID: string;
    folder: string;
    groups: number;
    pickerKind: PickerKind;
  };
  let confirmation = $state<PhotoCullConfirmation>();
  let checkpoint = $state<PhotoImportCheckpoint>();
  let recovery = $state<Recovery>();
  let pickerKind: "files" | "folder" = "files";
  // Only "Choose the same …" resumes the offered import; the regular picker starts a new one.
  let recoveryRequested = false;
  let resumes = false;
  const checkpointVaultID = $derived(vaultID);
  const checkpointSession = $derived(session);
  const storage = $derived(photoImportStorage(checkpointVaultID, checkpointSession));
  const settled = $derived(settledPhotoImports(checkpointVaultID));
  const lifecycle = new AbortController();
  let checkpointReady = $state(false);
  let lookupError = $state("");
  let retryLookup: (() => void) | undefined;
  let sessionOnly = $state(false);
  let storageNotice = $state("");
  $effect(() => {
    const id = checkpointVaultID;
    const currentStorage = photoImportStorage(id, checkpointSession);
    let obsolete = false;
    checkpointReady = false;
    lookupError = "";
    retryLookup = undefined;
    sessionOnly = false;
    storageNotice = "";
    checkpoint = undefined;
    recovery = undefined;
    controller?.abort();
    items = [];
    unsupported = [];
    selectedFiles = [];
    interruptedPlan = undefined;
    plannedDestination = "";
    error = "";
    if (id) {
      void currentStorage
        .load()
        .then((saved) => {
          if (obsolete || destroyed) return;
          checkpoint = saved;
          void findRecovery(saved, () => obsolete);
        })
        .catch((cause) => {
          if (!obsolete && !destroyed) {
            if (useSessionOnly(cause)) void findRecovery(undefined, () => obsolete);
            else showError(cause);
          }
        });
    }
    return () => {
      obsolete = true;
    };
  });

  let picker = $state<LocalFilePicker>();
  let destination = $state("/");
  let items = $state<Item[]>([]);
  let selectedFiles: File[] = [];
  let unsupported = $state<{ file: File; reason: string }[]>([]);
  let importID = $state("");
  let sourceID = $state("");
  let plannedChannel: PhotoImportTransport | undefined;
  let interruptedPlan = $state.raw<PhotoImportTransport>();
  let plannedDestination = $state("");
  let running = $state(false);
  let planning = $state(false);
  let hashProgress = $state<TransferProgress>();
  let stopping = $state(false);
  let error = $state("");
  let controller: AbortController | null = null;
  let destroyed = false;
  $effect(() => {
    if (interruptedPlan && interruptedPlan !== channel && !planning && !disabledReason) {
      interruptedPlan = undefined;
      void plan();
    }
  });
  let counts = $state<Record<Item["state"], number>>({
    ready: 0,
    hashing: 0,
    uploading: 0,
    added: 0,
    skipped: 0,
    ambiguous: 0,
    failed: 0,
    unconfirmed: 0,
  });
  const pending = $derived(counts.ready + counts.failed + counts.unconfirmed);
  const needsDestination = $derived(!destination.trim());
  const started = $derived(counts.ready < items.length);
  const recovering = $derived(Boolean(recovery && items.length && sourceID === recovery.sourceID));
  const recoveryGroupSummary = $derived(
    recovering
      ? counts.skipped > 0
        ? `${counts.skipped} of ${items.length} group${items.length === 1 ? "" : "s"} already in the vault`
        : ""
      : recovery && recovery.groups > 0
        ? `${recovery.groups} group${recovery.groups === 1 ? "" : "s"} already in the vault`
        : "",
  );
  const totals = $derived(
    (["added", "skipped", "ambiguous", "failed", "unconfirmed"] as const).filter(
      (state) => counts[state] > 0 && !(recovering && state === "skipped"),
    ),
  );
  const recoverySummary = $derived(
    recovery
      ? [
          recovery.folder
            ? `Folder “${recovery.folder}”`
            : recovery.pickerKind === "folder"
              ? "Import from a folder"
              : "Import of chosen files",
          recoveryGroupSummary,
        ]
          .filter(Boolean)
          .join(" · ")
      : "",
  );
  function setState(item: Item, state: Item["state"]): void {
    if (item.state === state) return;
    counts[item.state]--;
    counts[state]++;
    item.state = state;
  }
  const labels = {
    ready: "Ready",
    hashing: "Hashing",
    uploading: "Uploading",
    added: "Added",
    skipped: "Already in vault",
    ambiguous: "Ambiguous",
    failed: "Failed",
    unconfirmed: "Unconfirmed",
  };
  onDestroy(() => {
    destroyed = true;
    controller?.abort();
    lifecycle.abort();
  });

  // A saved checkpoint names its import; otherwise the latest recorded import is offered.
  // Until that lookup answers, a fresh import could take a new ID and add an interrupted import's sidecars again.
  async function findRecovery(
    saved: PhotoImportCheckpoint | undefined,
    obsolete: () => boolean,
  ): Promise<void> {
    lookupError = "";
    retryLookup = undefined;
    let found = null;
    try {
      found = (
        await getBrowserPhotoImport(saved ? { source_id: saved.sourceID } : undefined, {
          session,
          signal: lifecycle.signal,
        })
      ).import;
    } catch (cause) {
      if (obsolete() || destroyed) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
      if (!saved) {
        lookupError = `Could not check the vault for an interrupted import. ${cause instanceof Error ? cause.message : String(cause)}`;
        retryLookup = () => void findRecovery(saved, obsolete);
        return;
      }
    }
    if (obsolete() || destroyed) return;
    checkpointReady = true;
    if (items.length || planning || running) return;
    if (!saved && (!found || settled.get() === found.source_id)) return;
    const pickerKind = (saved ? saved.pickerKind === "folder" : found!.folder_selection)
      ? "folder"
      : "files";
    recovery = {
      sourceID: saved?.sourceID ?? found!.source_id,
      folder: pickerKind === "folder" ? (found?.folder ?? "") : "",
      groups: found?.groups ?? 0,
      pickerKind,
    };
    destination = saved?.destination ?? "";
  }

  function recover(): void {
    if (!recovery || needsDestination) return;
    recoveryRequested = true;
    picker?.choose(recovery.pickerKind);
  }

  function clearSelection(): void {
    items = [];
    for (const state of Object.keys(counts) as Item["state"][]) counts[state] = 0;
    unsupported = [];
    selectedFiles = [];
    sourceID = "";
    plannedDestination = "";
  }

  async function settle(): Promise<void> {
    if (checkpoint && !sessionOnly) {
      try {
        await storage.replace(undefined, checkpoint);
      } catch (cause) {
        if (!useSessionOnly(cause)) throw cause;
      }
    }
    settled.set(recovery?.sourceID ?? sourceID);
    checkpoint = undefined;
    recovery = undefined;
  }

  async function dismiss(): Promise<void> {
    if (!recovery || running || planning) return;
    const recovered = recovering;
    try {
      await settle();
    } catch (cause) {
      showError(cause);
      return;
    }
    if (recovered) clearSelection();
  }

  async function choose(files: File[], kind: PickerKind): Promise<void> {
    if (!files.length || running || planning || !checkpointReady) return;
    try {
      photoImportDeclaration(files, destination);
    } catch (cause) {
      showError(cause);
      return;
    }
    const resume = Boolean(recovery) && recoveryRequested;
    recoveryRequested = false;
    const expected = checkpoint;
    if (expected && !resume) {
      planning = true;
      const confirmed = await confirmation?.confirm(
        "Replace the unfinished import?",
        "Replace import",
        "The current selection and its progress will be discarded.",
      );
      planning = false;
      if (!confirmed || destroyed || checkpoint !== expected) return;
    }
    pickerKind = kind;
    resumes = resume;
    interruptedPlan = undefined;
    sourceID = "";
    selectedFiles = files;
    await plan(true);
  }

  async function plan(validated = false, reconnect = false): Promise<void> {
    if (!selectedFiles.length || running || planning || !checkpointReady) return;
    if (!validated) {
      try {
        photoImportDeclaration(selectedFiles, destination);
      } catch (cause) {
        showError(cause);
        return;
      }
    }
    const files = selectedFiles;
    const previousItems = reconnect ? items : [];
    const planChannel = channel;
    const target = destination;
    const currentStorage = storage;
    const expected = checkpoint;
    const active = new AbortController();
    controller = active;
    planning = true;
    if (!reconnect) {
      items = [];
      for (const state of Object.keys(counts) as Item["state"][]) counts[state] = 0;
      unsupported = [];
    }
    error = "";
    try {
      const { digest } = photoImportManifest(files);
      const resumed = resumes ? recovery?.sourceID : undefined;
      if (
        resumed &&
        checkpoint &&
        (checkpoint.destination !== target ||
          checkpoint.manifestVersion !== 2 ||
          checkpoint.manifestHash !== digest ||
          checkpoint.pickerKind !== pickerKind)
      )
        throw new Error(
          "These files or destination differ from the saved import. Reselect the original files or folder and destination.",
        );
      const nextSourceID = reconnect ? sourceID : (resumed ?? randomUUID());
      const next = reconnect
        ? undefined
        : {
            sourceID: nextSourceID,
            revision: randomUUID(),
            destination: target,
            manifestVersion: 2,
            manifestHash: digest,
            pickerKind,
          };
      const recoveryArgs: [Map<File, string>?, boolean?] = reconnect
        ? [
            new Map(
              previousItems
                .flatMap((item) =>
                  item.files.map((file, index) => [file, item.hashes[index]] as const),
                )
                .filter(([, hash]) => !!hash),
            ),
            true,
          ]
        : resumed
          ? [undefined, true]
          : [];
      const plan = await planChannel.planPhotoImport(
        files,
        target,
        nextSourceID,
        active.signal,
        (progress) => {
          hashProgress = progress;
        },
        ...recoveryArgs,
      );
      hashProgress = undefined;
      if (
        reconnect &&
        (active.signal.aborted || destroyed || channel !== planChannel || destination !== target)
      )
        return;
      if (!reconnect && !sessionOnly) {
        try {
          await currentStorage.replace(plan.groups.length ? next : undefined, expected);
        } catch (cause) {
          if (!useSessionOnly(cause)) throw cause;
        }
      }
      if (active.signal.aborted || destroyed || destination !== target) return;
      if (!reconnect) checkpoint = plan.groups.length ? next : undefined;
      // A new import replaced the offered one's saved progress, so it is no longer offered here.
      if (!resumed) recovery = undefined;
      plannedDestination = target;
      sourceID = nextSourceID;
      plannedChannel = planChannel;
      // Groups the vault already holds from this source never resend; an unconfirmed group is decided by this plan.
      const present = new Set(plan.present ?? []);
      items = plan.groups.map((group, index) => {
        const groupFiles = group.map((index) => files[index]);
        const previous =
          previousItems[index]?.state === "unconfirmed" ? undefined : previousItems[index];
        return {
          group: index,
          files: groupFiles,
          hashes: group.map(
            (fileIndex, member) =>
              previous?.hashes[member] || plan.hashes?.get(files[fileIndex]) || "",
          ),
          state: previous?.state ?? (present.has(index) ? "skipped" : "ready"),
          error: previous?.error,
          progress: previous?.progress ?? {
            processed: 0,
            total: groupFiles.reduce((sum, file) => sum + file.size, 0),
          },
        };
      });
      for (const state of Object.keys(counts) as Item["state"][])
        counts[state] = items.filter((item) => item.state === state).length;
      importID = plan.import_id;
      unsupported = plan.unsupported.map((index) => ({
        file: files[index],
        reason: plan.skip_reasons?.[index] ?? "Skipped",
      }));
      if (resumed && items.every((item) => item.state === "skipped")) await settle();
    } catch (cause) {
      if (!reconnect && cause instanceof UploadChannelError && !active.signal.aborted && !destroyed)
        interruptedPlan = planChannel;
      if (!active.signal.aborted && !destroyed) showError(cause);
    } finally {
      hashProgress = undefined;
      planning = false;
      controller = null;
    }
  }

  function useSessionOnly(cause: unknown): boolean {
    if (!photoImportStorageUnavailable(cause)) return false;
    sessionOnly = true;
    storageNotice = "Import progress is not saved in this browser.";
    return true;
  }

  function showError(cause: unknown): void {
    error = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
  }

  async function start(): Promise<void> {
    if (running || planning || disabledReason || !checkpointReady || !pending) return;
    const runChannel = channel;
    if (plannedDestination !== destination || plannedChannel !== runChannel || counts.unconfirmed) {
      await plan(false, plannedDestination === destination && Boolean(sourceID));
    }
    if (
      destroyed ||
      running ||
      planning ||
      !checkpointReady ||
      !pending ||
      disabledReason ||
      plannedDestination !== destination ||
      plannedChannel !== runChannel ||
      channel !== runChannel ||
      counts.unconfirmed
    )
      return;
    const runImportID = importID;
    running = true;
    stopping = false;
    controller = new AbortController();
    let refreshNeeded = false;
    let added = false;
    let failedCause: unknown;
    try {
      for (const item of items) {
        if (stopping || disabledReason || controller.signal.aborted) break;
        if (item.state !== "ready" && item.state !== "failed") continue;
        if (!sessionOnly) {
          try {
            if ((await storage.saved())?.revision !== checkpoint?.revision)
              throw new APIError(
                "Import progress changed in another tab. Close and reopen Add photos.",
                409,
                "checkpoint_changed",
              );
          } catch (cause) {
            if (!useSessionOnly(cause)) throw cause;
          }
        }
        item.error = undefined;
        setState(item, item.hashes.includes("") ? "hashing" : "uploading");
        let transmissionStarted = false;
        try {
          const receipt = await runChannel.uploadPhotoGroup(
            runImportID,
            item.group,
            item.files,
            controller.signal,
            (phase, progress) => {
              setState(item, phase);
              item.progress = progress;
              if (phase === "uploading" && (progress.processed > 0 || progress.submitted)) {
                transmissionStarted = true;
                refreshNeeded = true;
              }
            },
            item.hashes,
          );
          added ||= receipt.status === "added";
          setState(item, receipt.status);
          item.error =
            receipt.reason === "multiple_raw"
              ? "Multiple RAW files share this name."
              : receipt.reason === "separate_photos"
                ? "These files belong to separate photos."
                : receipt.reason;
          refreshNeeded = true;
        } catch (cause) {
          if (controller.signal.aborted && !transmissionStarted) break;
          const state =
            transmissionStarted && !(cause instanceof APIError) ? "unconfirmed" : "failed";
          setState(item, state);
          item.error =
            state === "unconfirmed"
              ? "Outcome unknown. The vault is checked before this group is sent again."
              : cause instanceof Error
                ? cause.message
                : String(cause);
          failedCause ??= cause;
          if (cause instanceof APIError && cause.status === 401) {
            onauthfailure(cause);
            break;
          }
          if (cause instanceof UploadChannelError || controller.signal.aborted) break;
          if (
            cause instanceof APIError &&
            ["not_dir", "not_found", "audit_mutation_unsupported", "maintenance_busy"].includes(
              cause.code,
            )
          )
            break;
        }
      }
      if (items.every((item) => ["added", "skipped", "ambiguous"].includes(item.state)))
        await settle();
    } catch (cause) {
      failedCause ??= cause;
      showError(cause);
    } finally {
      if (added) reportAction(session, "document_added");
      if (failedCause) reportFailure(session, "add", failedCause);
      running = false;
      controller = null;
      if (refreshNeeded) {
        try {
          await oncomplete();
        } catch (cause) {
          showError(cause);
        }
      }
    }
  }
</script>

{#snippet channelFooter()}
  <UploadChannelNotice state={connectionState} message={disabledReason} size="md" {onreconnect} />
{/snippet}

{#snippet addActions()}
  {#if running}<Button disabled={stopping} onclick={() => (stopping = true)}
      >{stopping ? "Stopping after this group" : "Stop after this group"}</Button
    >{/if}
  {#if pending}<Button
      size={recovering ? "sm" : undefined}
      tone="info"
      surface="solid"
      disabled={!checkpointReady || running || planning || !!disabledReason}
      onclick={() => void start()}
      >Add {pending}{recovering ? " remaining" : ""} group{pending === 1 ? "" : "s"}</Button
    >{/if}
{/snippet}

{#snippet selection()}
  {#if totals.length || unsupported.length}<div class="totals" aria-live="polite">
      {#each totals as state, index}
        {#if index}<span aria-hidden="true">·</span>{/if}<span
          >{labels[state]}: {counts[state]}</span
        >
      {/each}
      {#if unsupported.length}
        {#if totals.length}<span aria-hidden="true">·</span>{/if}
        <span>Skipped: {unsupported.length}</span>
      {/if}
    </div>{/if}
  <ul class:photo-import-recovery-list={recovering}>
    {#each items.filter((item) => !(recovering && item.state === "skipped")) as item}
      <li>
        <strong>{item.files.map(localFilePath).join(", ")}</strong>
        {#if item.state !== "ready"}<span>{labels[item.state]}</span>{/if}
        {#if item.state === "hashing" || item.state === "uploading"}<progress
            max={item.progress.total || 1}
            value={item.progress.processed}
            aria-label={labels[item.state]}
          ></progress>{/if}
        {#if item.error}<p role="alert">{item.error}</p>{/if}
      </li>
    {/each}
    {#each unsupported as { file, reason }}<li>
        <strong>{localFilePath(file)}</strong><span>{reason}</span>
      </li>{/each}
  </ul>
{/snippet}

<DrawerShell
  title="Add photos"
  closeLabel="Close add photos"
  {onclose}
  footer={disabledReason ? channelFooter : undefined}
>
  <div
    class="tool-body"
    class:photo-import-planned={items.length > 0 && !recovery}
    class:photo-import-recovery={recovery}
  >
    <label for="photo-destination"
      >Destination<TextInput
        id="photo-destination"
        ariaLabel="Destination"
        bind:value={destination}
        onchange={() => {
          if (!items.length && destination !== plannedDestination) void plan();
        }}
        disabled={!checkpointReady || running || planning || started}
        block
      /></label
    >
    {#if recovery}
      <section
        class="tool-section photo-import-recovery-section"
        aria-labelledby="photo-recovery-heading"
      >
        <h3 id="photo-recovery-heading">Interrupted import found</h3>
        {#if error}<p role="alert">{error}</p>{/if}
        {#if recovering}
          <div class="photo-import-recovery-plan">
            <p>{recoverySummary}</p>
            {@render selection()}
          </div>
        {:else}
          <p>{recoverySummary}</p>
          {#if needsDestination}<p>Choose the original destination for this import.</p>{/if}
        {/if}
        <div class="tool-actions tool-actions--start">
          {#if recovering}
            {@render addActions()}
          {:else}
            <Button
              size="sm"
              tone="info"
              surface="solid"
              disabled={!checkpointReady ||
                running ||
                planning ||
                needsDestination ||
                !!disabledReason}
              onclick={recover}
              >Choose the same {recovery.pickerKind === "folder" ? "folder" : "files"}…</Button
            >
          {/if}
          {#if !running}<Button
              size="sm"
              surface="soft"
              disabled={planning}
              onclick={() => void dismiss()}>Dismiss</Button
            >{/if}
        </div>
        {#if recovering && running}<p class="tool-muted">
            Keep this tab open until the import finishes.
          </p>{/if}
      </section>
    {/if}
    <section
      class="tool-section"
      class:photo-import-source-separated={recovery}
      aria-label="Photo source"
    >
      {#if recovery}<h3>Start a new import</h3>{/if}
      {#if lookupError}
        <p role="alert">{lookupError}</p>
        <div class="tool-actions">
          <Button onclick={() => retryLookup?.()}>Check again</Button>
        </div>
      {:else if !recovery && !disabledReason && error}
        <p role="alert">{error}</p>
      {:else if !disabledReason && !checkpointReady}
        <p role="status">{vaultID ? "Loading saved import…" : "Waiting for vault identity…"}</p>
      {/if}
      {#if storageNotice}<p role="status">{storageNotice}</p>{/if}
      {#if planning}
        <p role="status">{hashProgress ? "Hashing" : "Planning"}</p>
        {#if hashProgress}<progress
            max={hashProgress.total || 1}
            value={hashProgress.processed}
            aria-label="Hashing"
          ></progress>{/if}
      {/if}
      <div class="controls">
        <LocalFilePicker
          bind:this={picker}
          disabled={!checkpointReady || running || planning || needsDestination || !!disabledReason}
          filesLabel="Choose photo files"
          folderLabel="Choose photo folder"
          onchoose={choose}
          onopen={() => (recoveryRequested = false)}
        />
      </div>
      {#if !recovery && !disabledReason && !error && running}<p class="tool-muted">
          Keep this tab open until the import finishes.
        </p>{/if}
    </section>
    {#if !recovering && (items.length || unsupported.length)}
      <section class="tool-section" aria-label="Selected photos">
        {@render selection()}
        {#if pending || running}<div class="tool-actions">
            {@render addActions()}
          </div>{/if}
      </section>
    {/if}
  </div>
</DrawerShell>

<PhotoCullConfirmation bind:this={confirmation} />

<style>
  .controls,
  .totals {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .tool-body.photo-import-planned,
  .tool-body.photo-import-recovery {
    gap: var(--space-5);
  }
  .photo-import-source-separated {
    border-top: 1px solid var(--border-default);
    padding-top: var(--space-5);
  }
  .photo-import-recovery-plan {
    display: grid;
    gap: var(--space-3);
  }
  .photo-import-recovery-section {
    gap: var(--space-3);
  }
  .photo-import-recovery-section .tool-actions {
    gap: var(--space-2);
  }
  .photo-import-recovery-list li {
    padding: 0;
    border: 0;
    border-radius: 0;
  }
  p {
    margin: 0;
  }
  label,
  li {
    display: grid;
    gap: var(--space-2);
  }
  ul {
    display: grid;
    list-style: none;
    padding: 0;
    margin: 0;
    gap: var(--space-3);
    max-height: 52vh;
    overflow-y: auto;
  }
  li {
    padding: var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    overflow-wrap: anywhere;
  }
  .totals,
  li span,
  p {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }
  [role="alert"] {
    color: var(--accent-red);
  }
  progress {
    width: 100%;
  }
</style>
