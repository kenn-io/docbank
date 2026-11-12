<script lang="ts">
  import UploadChannelNotice from "./UploadChannelNotice.svelte";
  import type { UploadConnectionState } from "./uploadReconnect.js";
  import { reportAction, reportFailure } from "./action-events.js";
  import { onDestroy } from "svelte";
  import { Button, TextInput } from "@kenn-io/kit-ui";
  import ClockIcon from "@lucide/svelte/icons/clock";
  import PlusIcon from "@lucide/svelte/icons/plus";
  import RotateCcwIcon from "@lucide/svelte/icons/rotate-ccw";
  import SearchIcon from "@lucide/svelte/icons/search";
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
  let moreImports = $state(false);
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
  const warningStates: Item["state"][] = ["failed", "unconfirmed", "missing", "changed"];

  function stateLabel(item: Item): string {
    if (item.importedEarlier && item.state === "added") return "Imported earlier";
    if (item.importedEarlier && item.state === "skipped") return "Already in vault at import";
    const single = (item.names ?? item.files).length === 1;
    if (item.state === "missing" && single) return "File missing";
    if (item.state === "changed" && single) return "File changed";
    return labels[item.state];
  }
  function itemName(item: Item): string {
    return (item.names ?? item.files.map(localFilePath)).join(", ");
  }
  function savedImportLabel(saved: SavedPhotoImport): string {
    const created = new Date(saved.created_at).toLocaleString(undefined, {
      month: "short",
      day: "numeric",
      hour: "numeric",
      minute: "2-digit",
    });
    const groups = `${saved.group_count} group${saved.group_count === 1 ? "" : "s"}`;
    const suggested = checkpoint?.sourceID === saved.source_id ? "Suggested · " : "";
    return `${suggested}${created} · ${groups} → ${saved.destination}`;
  }
  // Explains a disabled "Resume remaining" after files were chosen again.
  const resumeHint = $derived.by(() => {
    if (!savedImport || pending) return "";
    const missing = items.filter((item) => item.state === "missing").map(itemName);
    if (missing.length) return `Choose ${missing.join(", ")} again to resume.`;
    const changed = items.filter((item) => item.state === "changed").map(itemName);
    if (changed.length) return `Choose the original ${changed.join(", ")} to resume.`;
    return "";
  });

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
    if (resuming && checkpoint) {
      try {
        await inspectAndApply(checkpoint.sourceID, false, true);
        if (savedImport) await chooseSavedFiles(files);
        return;
      } catch (cause) {
        if (!(cause instanceof APIError && cause.status === 404)) {
          showError(cause);
          return;
        }
        if (checkpoint.protocolVersion === 3) {
          error = "This import is no longer available for inspection. Its outcome is unconfirmed.";
          return;
        }
      }
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

  async function listSavedImports(more = false): Promise<void> {
    if (disabledReason || running || planning) return;
    planning = true;
    try {
      const page = await channel.listPhotoImports(
        more ? savedImports.length : 0,
        new AbortController().signal,
      );
      savedImports = more ? [...savedImports, ...page] : page;
      moreImports = page.length === 50;
    } catch (cause) {
      showError(cause);
    } finally {
      planning = false;
    }
  }

  async function inspectAndApply(id: string, preserve: boolean, bySource = false): Promise<void> {
    const activeChannel = channel;
    const saved = await activeChannel.inspectPhotoImport(
      id,
      new AbortController().signal,
      bySource,
    );
    if (channel !== activeChannel || destroyed) return;
    if (preserve) {
      for (const [index, group] of (saved.groups ?? []).entries()) {
        const item = items[index];
        if (group.status === "committed") {
          setState(item, group.receipt!.status);
          item.error = receiptReason(group.receipt?.reason);
          item.importedEarlier = true;
        } else if (group.status === "remaining") {
          setState(item, item.files.length ? "ready" : "missing");
          item.error = undefined;
        } else setState(item, group.status);
      }
    } else {
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
    }
    savedImport = saved;
    selectedImport = saved.import_id;
    importID = saved.import_id;
    registered = true;
    registrationUncertain = false;
    destination = saved.destination;
    plannedDestination = destination;
    plannedChannel = activeChannel;
    if (saved.groups?.some((group) => group.status === "committed")) await oncomplete();
    if (
      saved.groups?.length &&
      saved.groups.every((group) => group.status === "committed") &&
      checkpoint?.sourceID === saved.source_id &&
      !sessionOnly
    ) {
      try {
        await storage.replace(undefined, checkpoint);
        checkpoint = undefined;
      } catch (cause) {
        if (!useSessionOnly(cause)) throw cause;
      }
    }
  }

  async function inspectSavedImport(): Promise<void> {
    if (!selectedImport || disabledReason || running || planning) return;
    planning = true;
    try {
      await inspectAndApply(selectedImport, false);
    } catch (cause) {
      showError(cause);
    } finally {
      planning = false;
    }
  }

  async function inspectCurrentImport(): Promise<void> {
    if ((!registered && !registrationUncertain) || running || planning || disabledReason) return;
    const activeChannel = channel;
    planning = true;
    try {
      await inspectAndApply(
        registrationUncertain ? sourceID : importID,
        true,
        registrationUncertain,
      );
    } catch (cause) {
      if (
        registrationUncertain &&
        cause instanceof APIError &&
        cause.status === 404 &&
        channel === activeChannel &&
        !destroyed
      ) {
        registrationUncertain = false;
        plannedChannel = undefined;
        error = "";
      } else showError(cause);
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
      registrationUncertain
    )
      return;
    const runChannel = channel;
    if (
      !savedImport &&
      !registered &&
      (plannedDestination !== destination || plannedChannel !== runChannel)
    ) {
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
      (!savedImport && !registered && plannedChannel !== runChannel) ||
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
      if (savedImport || (registered && plannedChannel !== runChannel)) {
        const latest = await channel.inspectPhotoImport(importID, controller.signal);
        for (const [index, group] of (latest.groups ?? []).entries()) {
          if (
            group.status === "committed" ||
            (items[index].state === "ready" && group.status !== "remaining")
          ) {
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
        plannedChannel = runChannel;
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
      if (
        !sessionOnly &&
        checkpoint &&
        checkpoint.protocolVersion !== 3 &&
        (!savedImport || checkpoint.sourceID === savedImport.source_id)
      ) {
        const next = { ...checkpoint, protocolVersion: 3, revision: randomUUID() };
        await storage.replace(next, checkpoint);
        checkpoint = next;
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
          if (savedImport?.groups) {
            savedImport.groups[item.group] = {
              ...savedImport.groups[item.group],
              status: "committed",
              receipt,
            };
          }
          refreshNeeded = true;
        } catch (cause) {
          if (controller.signal.aborted && !transmissionStarted) break;
          const state =
            transmissionStarted && !(cause instanceof APIError) ? "unconfirmed" : "failed";
          setState(item, state);
          item.error =
            state === "unconfirmed"
              ? "Import outcome is unconfirmed."
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
        size="sm"
        surface="soft"
        disabled={running || planning || !!disabledReason}
        onclick={() => void listSavedImports()}><ClockIcon size="14" />Saved imports</Button
      >
      {#if savedImports.length}
        {#if moreImports}<Button
            size="sm"
            surface="soft"
            disabled={running || planning || !!disabledReason}
            onclick={() => void listSavedImports(true)}>Load more</Button
          >{/if}
        <select
          bind:value={selectedImport}
          aria-label="Saved import"
          disabled={running || planning}
        >
          <option value="">Choose an import</option>
          {#each savedImports as saved}
            <option value={saved.import_id}>{savedImportLabel(saved)}</option>
          {/each}
        </select>
        <Button
          size="sm"
          surface="soft"
          disabled={!selectedImport || running || planning || !!disabledReason}
          onclick={() =>
            void (selectedImport === importID && registered
              ? inspectCurrentImport()
              : inspectSavedImport())}><SearchIcon size="14" />Inspect import</Button
        >
      {/if}
      {#if savedImport || registered || registrationUncertain}
        <Button
          size="sm"
          surface="soft"
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
          }}><PlusIcon size="14" />Start new import</Button
        >
      {/if}
      {#if checkpoint && !items.length && !savedImport && !registered && !registrationUncertain}
        <Button
          size="sm"
          surface="soft"
          disabled={running || planning || !!disabledReason}
          onclick={() => {
            resuming = true;
            destination = checkpoint!.destination;
            picker.choose(checkpoint!.pickerKind === "folder" ? "folder" : "files");
          }}><RotateCcwIcon size="14" />Resume import</Button
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
            class:zero={!counts[state as Item["state"]]}
            >{labels[state as keyof typeof labels]}: {counts[state as Item["state"]]}</span
          >{/each}
        <span class:zero={!unsupported.length}>Skipped: {unsupported.length}</span>
      </div>
      <ul>
        {#each items as item}
          <li>
            <strong>{itemName(item)}</strong><span
              class:warning={warningStates.includes(item.state)}
              class:ready={item.state === "ready"}>{stateLabel(item)}</span
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
        {#if ((registered && (counts.unconfirmed > 0 || counts.pending > 0)) || registrationUncertain) && !(savedImports.length && selectedImport === importID && registered)}
          <Button
            size="sm"
            surface="soft"
            disabled={running || planning || !!disabledReason}
            onclick={() => void inspectCurrentImport()}
            ><SearchIcon size="14" />Inspect import</Button
          >
        {/if}
        {#if !savedImport || items.some((item) => !["added", "skipped", "ambiguous"].includes(item.state))}
          <Button
            size="sm"
            tone="info"
            surface="solid"
            disabled={!checkpointReady ||
              running ||
              planning ||
              !pending ||
              !!disabledReason ||
              registrationUncertain}
            onclick={() => void start()}
            >{#if savedImport}<RotateCcwIcon size="14" />Resume remaining{:else}<PlusIcon
                size="14"
              />Add {pending} group{pending === 1 ? "" : "s"}{/if}</Button
          >
        {/if}
        {#if running}<Button
            size="sm"
            surface="soft"
            disabled={stopping}
            onclick={() => (stopping = true)}
            >{stopping ? "Stopping after this group" : "Stop after this group"}</Button
          >{/if}
      </div>
      {#if resumeHint}<p role="status">{resumeHint}</p>{/if}
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
  /* Match the small soft buttons beside it, including their rendered height. */
  select {
    box-sizing: border-box;
    align-self: stretch;
    min-height: 24px;
    padding: 2px var(--space-2);
    border: var(--border-width) solid color-mix(in srgb, var(--border-default) 80%, transparent);
    border-radius: var(--kit-control-radius, var(--radius-md));
    background: color-mix(in srgb, var(--text-muted) 8%, var(--bg-raised, var(--bg-inset)));
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }
  .totals {
    gap: var(--space-1) var(--space-2);
  }
  .totals span:not(:first-child)::before {
    content: "·";
    margin-right: var(--space-2);
    color: var(--text-muted);
  }
  .totals span:not(.zero) {
    color: var(--text-primary);
  }
  .totals .zero {
    color: var(--text-muted);
  }
  li span.ready {
    color: color-mix(in srgb, var(--accent-blue) 72%, var(--text-primary));
    font-weight: var(--font-weight-medium, 500);
  }
  li span.warning {
    color: var(--accent-amber);
    font-weight: var(--font-weight-medium, 500);
  }
  progress {
    width: 100%;
  }
</style>
