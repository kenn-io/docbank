<script lang="ts">
  import UploadChannelNotice from "./UploadChannelNotice.svelte";
  import type { UploadConnectionState } from "./uploadReconnect.js";
  import { reportAction, reportFailure } from "./action-events.js";
  import { onDestroy } from "svelte";
  import { Button, TextInput } from "@kenn-io/kit-ui";
  import DrawerShell from "./DrawerShell.svelte";
  import { APIError } from "./api-transport.js";
  import {
    UploadChannelError,
    photoImportDeclaration,
    type PhotoImportTransport,
    type PhotoGroupReceipt,
    type TransferProgress,
    type SavedPhotoImport,
    hashFile,
  } from "./upload.js";

  import LocalFilePicker from "./LocalFilePicker.svelte";
  import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";
  import { localFilePath, type PickerKind } from "./localFiles.js";

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
    names?: string[];
    importedEarlier?: boolean;
    hashes: string[];
    state:
      | "ready"
      | "hashing"
      | "uploading"
      | "unconfirmed"
      | "failed"
      | "pending"
      | "missing"
      | "changed"
      | PhotoGroupReceipt["status"];
    progress: TransferProgress;
    error?: string;
  };
  let checkpoint = $state<PhotoImportCheckpoint>();
  let resuming = false;
  let replacingConfirmed = false;
  let savedImports = $state<SavedPhotoImport[]>([]);
  let selectedImport = $state("");
  let savedImport = $state<SavedPhotoImport>();
  let registered = $state(false);
  let registrationUncertain = $state(false);
  let extraFiles = $state<string[]>([]);
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
    interruptedPlan = undefined;
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

  let confirmation = $state<PhotoCullConfirmation>();
  let picker: LocalFilePicker;
  let destination = $state("/");
  let items = $state<Item[]>([]);
  let selectedFiles: File[] = [];
  let unsupported = $state<{ name: string; reason: string }[]>([]);
  let importID = $state("");
  let sourceID = "";
  let plannedChannel: PhotoImportTransport | undefined;
  let interruptedPlan = $state.raw<PhotoImportTransport>();
  let reconnectPlanNeeded = $state(false);
  $effect(() => {
    const current = channel;
    if (
      plannedChannel &&
      current !== plannedChannel &&
      registered &&
      !savedImport &&
      !running &&
      !planning
    ) {
      reconnectPlanNeeded = true;
      for (const item of items) {
        if (item.state === "ready" || item.state === "failed") setState(item, "unconfirmed");
      }
    }
  });
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
    pending: 0,
    missing: 0,
    changed: 0,
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
    pending: "Still finishing",
    missing: "Files missing",
    changed: "Files changed",
  };

  function receiptReason(reason: string | undefined): string | undefined {
    if (reason === "multiple_raw") return "Multiple RAW files share this name.";
    if (reason === "separate_photos") return "These files belong to separate photos.";
    return reason;
  }
  onDestroy(() => {
    destroyed = true;
    controller?.abort();
  });

  async function choose(files: File[], kind: PickerKind): Promise<void> {
    if (!files.length || running || planning || !checkpointReady) return;
    if (savedImport) {
      await chooseSavedFiles(files);
      return;
    }
    try {
      photoImportDeclaration(files, destination);
    } catch (cause) {
      showError(cause);
      return;
    }
    const expected = checkpoint;
    if (expected && !resuming && !replacingConfirmed) {
      planning = true;
      const confirmed = await confirmation?.confirm(
        "Replace the unfinished import?",
        "Replace import",
        "The current selection and its progress will be discarded.",
      );
      planning = false;
      if (!confirmed || destroyed || checkpoint !== expected) return;
    }
    replacingConfirmed = false;
    pickerKind = kind;
    interruptedPlan = undefined;
    registered = false;
    registrationUncertain = false;
    sourceID = "";
    selectedFiles = files;
    await plan(true);
  }

  async function listSavedImports(): Promise<void> {
    if (disabledReason || running || planning) return;
    planning = true;
    try {
      const active = new AbortController();
      const all: SavedPhotoImport[] = [];
      for (let offset = 0; ; offset += 50) {
        const page = await channel.listPhotoImports(offset, active.signal);
        all.push(...page);
        if (page.length < 50) break;
      }
      savedImports = all;
    } catch (cause) {
      showError(cause);
    } finally {
      planning = false;
    }
  }

  async function inspectSavedImport(): Promise<void> {
    if (!selectedImport || disabledReason || running || planning) return;
    planning = true;
    try {
      const saved = await channel.inspectPhotoImport(
        selectedImport,
        false,
        new AbortController().signal,
      );
      savedImport = saved;
      registered = true;
      registrationUncertain = false;
      importID = saved.import_id;
      reconnectPlanNeeded = false;
      destination = saved.destination;
      plannedDestination = destination;
      plannedChannel = channel;
      selectedFiles = [];
      extraFiles = [];
      unsupported = (saved.skipped ?? []).map((file) => ({ name: file.path, reason: file.reason }));
      items = (saved.groups ?? []).map((group, index) => ({
        group: index,
        files: [],
        names: group.files.map((file) => file.relative_path),
        importedEarlier: group.status === "committed",
        hashes: group.hashes ?? group.files.map(() => ""),
        error: receiptReason(group.receipt?.reason),
        state:
          group.status === "committed"
            ? group.receipt!.status
            : group.status === "remaining"
              ? "missing"
              : group.status,
        progress: { processed: 0, total: group.files.reduce((sum, file) => sum + file.size, 0) },
      }));
      for (const state of Object.keys(counts) as Item["state"][])
        counts[state] = items.filter((item) => item.state === state).length;
      if (saved.groups?.some((group) => group.status === "committed")) await oncomplete();
    } catch (cause) {
      showError(cause);
    } finally {
      planning = false;
    }
  }

  async function chooseSavedFiles(files: File[]): Promise<void> {
    if (!savedImport) return;
    planning = true;
    try {
      const byPath = new Map(files.map((file) => [localFilePath(file), file]));
      const known = new Set([
        ...(savedImport.groups ?? []).flatMap((group) =>
          group.files.map((file) => file.relative_path),
        ),
        ...(savedImport.skipped ?? []).map((file) => file.path),
      ]);
      extraFiles = files.map(localFilePath).filter((name) => !known.has(name));
      const signal = new AbortController().signal;
      for (const [index, group] of (savedImport.groups ?? []).entries()) {
        const item = items[index];
        const matched = group.files.map((file) => byPath.get(file.relative_path));
        let changed = false;
        for (const [i, file] of matched.entries()) {
          if (!file) continue;
          if (file.size !== group.files[i].size) changed = true;
          if (
            !group.hashes?.[i] &&
            group.files[i].modified_time !== undefined &&
            file.lastModified !== group.files[i].modified_time
          )
            changed = true;
          if (group.hashes?.[i] && (await hashFile(file, signal, () => {})) !== group.hashes[i])
            changed = true;
        }
        if (group.status === "committed") {
          item.error = changed
            ? "Files changed since this group was imported."
            : receiptReason(group.receipt?.reason);
          continue;
        }
        if (group.status !== "remaining") continue;
        if (changed) setState(item, "changed");
        else if (matched.some((file) => !file)) setState(item, "missing");
        else {
          item.files = matched as File[];
          setState(item, "ready");
        }
      }
    } catch (cause) {
      showError(cause);
    } finally {
      planning = false;
    }
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
        sourceID: reconnect ? sourceID : resuming ? checkpoint!.sourceID : randomUUID(),
        revision: randomUUID(),
        destination: target,
        manifestVersion: 2,
        manifestHash: digest,
        pickerKind,
      };
      const plan = await planChannel.planPhotoImport(
        files,
        target,
        next.sourceID,
        active.signal,
        (progress) => {
          hashProgress = progress;
        },
        ...(reconnect
          ? ([
              new Map(
                previousItems
                  .flatMap((item) =>
                    item.files.map((file, index) => [file, item.hashes[index]] as const),
                  )
                  .filter(([, hash]) => !!hash),
              ),
            ] as const)
          : ([] as const)),
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
      if (!plan.groups.length) resuming = false;
      plannedDestination = target;
      sourceID = next.sourceID;
      plannedChannel = planChannel;
      items = plan.groups.map((group, index) => {
        const groupFiles = group.map((index) => files[index]);
        const previous = previousItems[index];
        return {
          group: index,
          files: groupFiles,
          hashes: group.map(
            (fileIndex, member) =>
              previous?.hashes[member] || plan.hashes?.get(files[fileIndex]) || "",
          ),
          state: previous?.state ?? "ready",
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
        name: localFilePath(files[index]),
        reason: plan.skip_reasons?.[index] ?? "Skipped",
      }));
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
    storageNotice = "Browser shortcuts are unavailable.";
    return true;
  }

  function showError(cause: unknown): void {
    error = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
  }

  async function start(): Promise<void> {
    if (
      running ||
      planning ||
      disabledReason ||
      !checkpointReady ||
      !pending ||
      reconnectPlanNeeded ||
      registrationUncertain
    )
      return;
    const runChannel = channel;
    if (!savedImport && (plannedDestination !== destination || plannedChannel !== runChannel)) {
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
      (!savedImport && plannedChannel !== runChannel) ||
      channel !== runChannel
    )
      return;
    running = true;
    stopping = false;
    controller = new AbortController();
    let refreshNeeded = false;
    let added = false;
    let failedCause: unknown;
    try {
      if (savedImport) {
        const latest = await channel.inspectPhotoImport(importID, true, controller.signal);
        for (const [index, group] of (latest.groups ?? []).entries()) {
          if (items[index].state === "ready" && group.status !== "remaining") {
            setState(
              items[index],
              group.status === "committed" ? group.receipt!.status : group.status,
            );
            if (group.status === "committed") {
              items[index].error = receiptReason(group.receipt?.reason);
              items[index].importedEarlier = true;
              refreshNeeded = true;
            }
          }
        }
        savedImport = latest;
      } else if (!registered) {
        registrationUncertain = true;
        try {
          importID = await channel.registerPhotoImport(importID, pickerKind, controller.signal);
        } catch (cause) {
          if (cause instanceof APIError) registrationUncertain = false;
          throw cause;
        }
        registered = true;
        registrationUncertain = false;
      }
      for (const item of items) {
        if (stopping || disabledReason || controller.signal.aborted) break;
        if (item.state !== "ready" && item.state !== "failed") continue;
        if (!sessionOnly && !savedImport) {
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
          item.error = receiptReason(receipt.reason);
          refreshNeeded = true;
        } catch (cause) {
          if (controller.signal.aborted && !transmissionStarted) break;
          const state =
            transmissionStarted && !(cause instanceof APIError) ? "unconfirmed" : "failed";
          setState(item, state);
          item.error =
            state === "unconfirmed"
              ? "Import outcome is unconfirmed. Open Saved imports, select this import, and choose Inspect import."
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
        if (
          !sessionOnly &&
          checkpoint &&
          (!savedImport || checkpoint.sourceID === savedImport.source_id)
        ) {
          try {
            await storage.replace(undefined, checkpoint);
            checkpoint = undefined;
          } catch (cause) {
            if (!useSessionOnly(cause)) throw cause;
          }
        }
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

{#snippet channelFooter()}
  <UploadChannelNotice state={connectionState} message={disabledReason} size="md" {onreconnect} />
{/snippet}

<DrawerShell
  title="Add photos"
  closeLabel="Close add photos"
  {onclose}
  footer={disabledReason ? channelFooter : undefined}
>
  <div class="photo-import">
    {#if !disabledReason && error}
      <p role="alert">{error}</p>
    {:else if !disabledReason && !checkpointReady}
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
        disabled={!checkpointReady || running || planning || started || !!savedImport}
        block
      /></label
    >
    <div class="controls">
      <Button
        disabled={running || planning || !!disabledReason}
        onclick={() => void listSavedImports()}>Saved imports</Button
      >
      {#if savedImports.length}
        <select
          bind:value={selectedImport}
          aria-label="Saved import"
          disabled={running || planning}
        >
          <option value="">Choose an import</option>
          {#each savedImports as saved}
            <option value={saved.import_id}
              >{checkpoint?.sourceID === saved.source_id ? "Suggested · " : ""}{saved.label.replace(
                /^browser: /,
                "",
              )} · {saved.destination} · {new Date(saved.created_at).toLocaleString()} · {saved.group_count}
              groups</option
            >
          {/each}
        </select>
        <Button
          disabled={!selectedImport || running || planning || !!disabledReason}
          onclick={() => void inspectSavedImport()}>Inspect import</Button
        >
      {/if}
      {#if savedImport || registered || registrationUncertain}
        <Button
          disabled={running || planning}
          onclick={() => {
            savedImport = undefined;
            replacingConfirmed = true;
            selectedImport = "";
            selectedFiles = [];
            items = [];
            registered = false;
            registrationUncertain = false;
            for (const state of Object.keys(counts) as Item["state"][]) counts[state] = 0;
          }}>Start new import</Button
        >
      {/if}
      {#if checkpoint && !items.length && !savedImport && !registered && !registrationUncertain}
        <Button
          disabled={running || planning || !!disabledReason}
          onclick={() => {
            resuming = true;
            destination = checkpoint!.destination;
            picker.choose(checkpoint!.pickerKind === "folder" ? "folder" : "files");
          }}>Resume import</Button
        >
      {/if}
      <LocalFilePicker
        bind:this={picker}
        disabled={!checkpointReady ||
          running ||
          planning ||
          !!disabledReason ||
          (registered && !savedImport) ||
          registrationUncertain}
        filesLabel="Choose photo files"
        folderLabel="Choose photo folder"
        onopen={() => {
          resuming = false;
        }}
        onchoose={choose}
      />
    </div>
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
            <strong>{(item.names ?? item.files.map(localFilePath)).join(", ")}</strong><span
              >{item.importedEarlier && item.state === "added"
                ? "Imported earlier"
                : item.importedEarlier && item.state === "skipped"
                  ? "Already in vault at import"
                  : labels[item.state]}</span
            >
            {#if item.state === "hashing" || item.state === "uploading"}<progress
                max={item.progress.total || 1}
                value={item.progress.processed}
                aria-label={labels[item.state]}
              ></progress>{/if}
            {#if item.error}<p role="alert">{item.error}</p>{/if}
          </li>
        {/each}
        {#each unsupported as { name, reason }}<li>
            <strong>{name}</strong><span>{reason}</span>
          </li>{/each}
        {#each extraFiles as name}<li>
            <strong>{name}</strong><span>Outside this import</span>
          </li>{/each}
      </ul>
      <div class="controls">
        {#if !savedImport || items.some((item) => !["added", "skipped", "ambiguous"].includes(item.state))}
          <Button
            disabled={!checkpointReady ||
              running ||
              planning ||
              !pending ||
              !!disabledReason ||
              reconnectPlanNeeded ||
              registrationUncertain}
            onclick={() => void start()}
            >{savedImport
              ? "Resume remaining"
              : `Add ${pending} group${pending === 1 ? "" : "s"}`}</Button
          >
        {/if}
        {#if running}<Button disabled={stopping} onclick={() => (stopping = true)}
            >{stopping ? "Stopping after this group" : "Stop after this group"}</Button
          >{/if}
      </div>
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
