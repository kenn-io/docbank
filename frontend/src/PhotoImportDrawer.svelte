<script lang="ts">
  import { reportAction, reportFailure } from "./action-events.js";
  import { onDestroy } from "svelte";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, DetailDrawer, IconButton, TextInput } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import {
    UploadChannelError,
    photoImportDeclaration,
    type PhotoImportTransport,
    type PhotoGroupReceipt,
    type TransferProgress,
  } from "./upload.js";

  import LocalFilePicker from "./LocalFilePicker.svelte";
  import { localFilePath, type PickerKind } from "./localFiles.js";
  import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";

  import { randomUUID } from "./crypto.js";
  import {
    photoImportManifest,
    photoImportStorage,
    photoImportStorageUnavailable,
    type PhotoImportCheckpoint,
  } from "./photoImportStorage.js";

  let {
    channel,
    vaultID,
    session,
    disabledReason = "",
    onclose,
    oncomplete,
    onauthfailure,
  }: {
    channel: PhotoImportTransport;
    vaultID: string;
    session: string;
    disabledReason?: string;
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
  let confirmation = $state<PhotoCullConfirmation>();
  let checkpoint = $state<PhotoImportCheckpoint>();
  let resuming = false;
  let pickerKind: "files" | "folder" = "files";
  const checkpointVaultID = $derived(vaultID);
  const checkpointSession = $derived(session);
  const storage = $derived(photoImportStorage(checkpointVaultID, checkpointSession));
  let checkpointReady = $state(false);
  let sessionOnly = $state(false);
  let storageNotice = $state("");
  $effect(() => {
    const id = checkpointVaultID;
    const currentStorage = photoImportStorage(id, checkpointSession);
    let obsolete = false;
    checkpointReady = false;
    sessionOnly = false;
    storageNotice = "";
    checkpoint = undefined;
    controller?.abort();
    items = [];
    unsupported = [];
    resuming = false;
    selectedFiles = [];
    plannedDestination = "";
    error = "";
    if (id) {
      void currentStorage
        .load()
        .then((saved) => {
          if (obsolete || destroyed) return;
          checkpoint = saved;
          checkpointReady = true;
        })
        .catch((cause) => {
          if (!obsolete && !destroyed) {
            if (useSessionOnly(cause)) checkpointReady = true;
            else showError(cause);
          }
        });
    }
    return () => {
      obsolete = true;
    };
  });

  let picker: LocalFilePicker;
  let destination = $state("/");
  let items = $state<Item[]>([]);
  let selectedFiles: File[] = [];
  let unsupported = $state<{ file: File; reason: string }[]>([]);
  let importID = $state("");
  let plannedDestination = $state("");
  let running = $state(false);
  let planning = $state(false);
  let hashProgress = $state<TransferProgress>();
  let stopping = $state(false);
  let error = $state("");
  let controller: AbortController | null = null;
  let destroyed = false;
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
  const pending = $derived(counts.ready + counts.failed);
  const started = $derived(counts.ready < items.length);
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
  });

  async function choose(files: File[], kind: PickerKind): Promise<void> {
    if (!files.length || running || planning || !checkpointReady) return;
    try {
      photoImportDeclaration(files, destination);
    } catch (cause) {
      showError(cause);
      return;
    }
    const expected = checkpoint;
    if (expected && !resuming) {
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
    selectedFiles = files;
    await plan(true);
  }

  async function plan(validated = false): Promise<void> {
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
    const target = destination;
    const currentStorage = storage;
    const expected = checkpoint;
    const active = new AbortController();
    controller = active;
    planning = true;
    plannedDestination = "";
    items = [];
    for (const state of Object.keys(counts) as Item["state"][]) counts[state] = 0;
    unsupported = [];
    error = "";
    try {
      const { digest } = photoImportManifest(files);
      if (
        resuming &&
        (!checkpoint ||
          checkpoint.destination !== target ||
          checkpoint.manifestVersion !== 2 ||
          checkpoint.manifestHash !== digest ||
          checkpoint.pickerKind !== pickerKind)
      )
        throw new Error(
          "These files or destination differ from the saved import. Reselect the original files or folder and destination.",
        );
      const next = {
        sourceID: resuming ? checkpoint!.sourceID : randomUUID(),
        revision: randomUUID(),
        destination: target,
        manifestVersion: 2,
        manifestHash: digest,
        pickerKind,
      };
      const plan = await channel.planPhotoImport(
        files,
        target,
        next.sourceID,
        active.signal,
        (progress) => {
          hashProgress = progress;
        },
      );
      hashProgress = undefined;
      if (!sessionOnly) {
        try {
          await currentStorage.replace(plan.groups.length ? next : undefined, expected);
        } catch (cause) {
          if (!useSessionOnly(cause)) throw cause;
        }
      }
      if (active.signal.aborted || destroyed) return;
      checkpoint = plan.groups.length ? next : undefined;
      if (!plan.groups.length) resuming = false;
      plannedDestination = target;
      items = plan.groups.map((group, index) => ({
        group: index,
        files: group.map((index) => files[index]),
        hashes: group.map((index) => plan.hashes?.get(files[index]) ?? ""),
        state: "ready",
        progress: { processed: 0, total: group.reduce((sum, index) => sum + files[index].size, 0) },
      }));
      counts.ready = items.length;
      importID = plan.import_id;
      unsupported = plan.unsupported.map((index) => ({
        file: files[index],
        reason: plan.skip_reasons?.[index] ?? "Skipped",
      }));
    } catch (cause) {
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
    storageNotice = "Resume is unavailable for this import.";
    return true;
  }

  function showError(cause: unknown): void {
    error = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
  }

  async function start(): Promise<void> {
    if (running || planning || disabledReason || !checkpointReady) return;
    if (plannedDestination !== destination) await plan();
    if (
      destroyed ||
      running ||
      planning ||
      !checkpointReady ||
      !pending ||
      disabledReason ||
      plannedDestination !== destination
    )
      return;
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
          const receipt = await channel.uploadPhotoGroup(
            importID,
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
              ? sessionOnly
                ? "Import outcome is unconfirmed. Run docbank web again, then choose the same files or folder again. Photos and videos already in the vault are skipped. Other files, including standalone XMP sidecars, may be added again."
                : "Import outcome is unconfirmed. Reload the page, then choose Resume import and reselect the same files or folder."
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
      if (items.every((item) => ["added", "skipped", "ambiguous"].includes(item.state))) {
        if (!sessionOnly) {
          try {
            await storage.replace(undefined, checkpoint);
          } catch (cause) {
            if (!useSessionOnly(cause)) throw cause;
          }
        }
        checkpoint = undefined;
      }
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

<DetailDrawer width="min(720px, 100vw)" ariaLabel="Add photos" {onclose}>
  {#snippet header()}
    <div class="drawer-heading">
      <h2>Add photos</h2>
      <IconButton ariaLabel="Close add photos" onclick={onclose}><XIcon size="18" /></IconButton>
    </div>
  {/snippet}
  <div class="photo-import">
    {#if disabledReason || error}
      <p role="alert">{disabledReason || error}</p>
    {:else if !checkpointReady}
      <p role="status">{vaultID ? "Loading saved import…" : "Waiting for vault identity…"}</p>
    {/if}
    {#if storageNotice}<p role="status">{storageNotice}</p>{/if}
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
    <div class="controls">
      {#if checkpoint && !sessionOnly}
        <Button
          disabled={!checkpointReady || running || planning || !!disabledReason}
          onclick={() => {
            resuming = true;
            destination = checkpoint!.destination;
            picker.choose(checkpoint!.pickerKind === "folder" ? "folder" : "files");
          }}>Resume import</Button
        >
      {/if}
      <LocalFilePicker
        bind:this={picker}
        disabled={!checkpointReady || running || planning || !!disabledReason}
        filesLabel="Choose photo files"
        folderLabel="Choose photo folder"
        onopen={() => {
          resuming = false;
        }}
        onchoose={choose}
      />
    </div>
    {#if !disabledReason && !error}<p>Keep this tab open until the import finishes.</p>{/if}
    {#if planning}
      <p role="status">{hashProgress ? "Hashing" : "Planning"}</p>
      {#if hashProgress}<progress
          max={hashProgress.total || 1}
          value={hashProgress.processed}
          aria-label="Hashing"
        ></progress>{/if}
    {/if}
    {#if items.length || unsupported.length}
      <div class="totals" aria-live="polite">
        {#each ["added", "skipped", "ambiguous", "failed", "unconfirmed"] as state}<span
            >{labels[state as keyof typeof labels]}: {counts[state as Item["state"]]}</span
          >{/each}
        <span>Skipped: {unsupported.length}</span>
      </div>
      <ul>
        {#each items as item}
          <li>
            <strong>{item.files.map(localFilePath).join(", ")}</strong><span
              >{labels[item.state]}</span
            >
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
      <div class="controls">
        <Button
          disabled={!checkpointReady || running || planning || !pending || !!disabledReason}
          onclick={() => void start()}>Add {pending} group{pending === 1 ? "" : "s"}</Button
        >
        {#if running}<Button disabled={stopping} onclick={() => (stopping = true)}
            >{stopping ? "Stopping after this group" : "Stop after this group"}</Button
          >{/if}
      </div>
    {/if}
  </div>
</DetailDrawer>

<PhotoCullConfirmation bind:this={confirmation} />

<style>
  .drawer-heading,
  .controls,
  .totals {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .drawer-heading {
    justify-content: space-between;
    width: 100%;
  }
  h2,
  p {
    margin: 0;
  }
  .photo-import {
    display: grid;
    padding: var(--space-5);
    gap: var(--space-4);
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
