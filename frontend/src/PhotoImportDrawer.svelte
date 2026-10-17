<script lang="ts">
  import { reportAction } from "./action-events.js";
  import { onDestroy } from "svelte";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, DetailDrawer, IconButton, TextInput } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import {
    UploadChannelError,
    type PhotoImportTransport,
    type PhotoGroupReceipt,
    type TransferProgress,
  } from "./upload.js";

  let {
    session,
    channel,
    disabledReason = "",
    onclose,
    oncomplete,
    onauthfailure,
  }: {
    session: string;
    channel: PhotoImportTransport;
    disabledReason?: string;
    onclose: () => void;
    oncomplete: () => void | Promise<void>;
    onauthfailure: (cause: unknown) => void;
  } = $props();
  type Item = {
    group: number;
    files: File[];
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
  let folderInput: HTMLInputElement;
  let filesInput: HTMLInputElement;
  let destination = $state("/");
  let items = $state<Item[]>([]);
  let selectedFiles: File[] = [];
  let unsupported = $state<{ file: File; reason: string }[]>([]);
  let importID = $state("");
  let plannedDestination = $state("");
  let running = $state(false);
  let planning = $state(false);
  let stopping = $state(false);
  let error = $state("");
  let controller: AbortController | null = null;
  let reportedAddition = false;
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

  async function choose(event: Event): Promise<void> {
    const input = event.currentTarget as HTMLInputElement;
    const files = Array.from(input.files ?? []);
    input.value = "";
    if (!files.length || running || planning) return;
    selectedFiles = files;
    await plan();
  }

  async function plan(): Promise<void> {
    if (!selectedFiles.length || running || planning) return;
    const files = selectedFiles;
    const target = destination;
    planning = true;
    plannedDestination = "";
    items = [];
    for (const state of Object.keys(counts) as Item["state"][]) counts[state] = 0;
    unsupported = [];
    error = "";
    try {
      const plan = await channel.planPhotoImport(files, target);
      plannedDestination = target;
      items = plan.groups.map((group, index) => ({
        group: index,
        files: group.map((index) => files[index]),
        state: "ready",
        progress: { processed: 0, total: group.reduce((sum, index) => sum + files[index].size, 0) },
      }));
      counts.ready = items.length;
      if (importID !== plan.import_id) reportedAddition = false;
      importID = plan.import_id;
      unsupported = plan.unsupported.map((index) => ({
        file: files[index],
        reason: plan.skip_reasons?.[index] ?? "Skipped",
      }));
    } catch (cause) {
      showError(cause);
    } finally {
      planning = false;
    }
  }

  function showError(cause: unknown): void {
    error = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
  }

  async function start(): Promise<void> {
    if (running || planning || disabledReason) return;
    if (plannedDestination !== destination) await plan();
    if (
      destroyed ||
      running ||
      planning ||
      !pending ||
      disabledReason ||
      plannedDestination !== destination
    )
      return;
    running = true;
    stopping = false;
    controller = new AbortController();
    let refreshNeeded = false;
    try {
      for (const item of items) {
        if (stopping || disabledReason || controller.signal.aborted) break;
        if (item.state !== "ready" && item.state !== "failed") continue;
        item.error = undefined;
        setState(item, "hashing");
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
          );
          if (receipt.status === "added" && !reportedAddition) {
            reportedAddition = true;
            reportAction(session, "document_added", "photos");
          }
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
              ? "Import outcome is unconfirmed. Reload the page and choose the folder again. Identical RAW, image, and video content returns as Already in vault. Other files, including lone XMP sidecars, may be added again."
              : cause instanceof Error
                ? cause.message
                : String(cause);
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
    } finally {
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
    <input
      bind:this={folderInput}
      type="file"
      webkitdirectory
      multiple
      hidden
      aria-label="Choose photo folder"
      onchange={(event) => void choose(event)}
    />
    <input
      bind:this={filesInput}
      type="file"
      multiple
      hidden
      aria-label="Choose photo files"
      onchange={(event) => void choose(event)}
    />
    {#if disabledReason || error}<p role="alert">{disabledReason || error}</p>{/if}
    <label for="photo-destination"
      >Destination<TextInput
        id="photo-destination"
        ariaLabel="Destination"
        bind:value={destination}
        onchange={() => {
          if (!items.length && destination !== plannedDestination) void plan();
        }}
        disabled={running || planning || started}
        block
      /></label
    >
    <div class="controls">
      <Button disabled={running || planning || !!disabledReason} onclick={() => folderInput.click()}
        >Choose folder</Button
      >
      <Button disabled={running || planning || !!disabledReason} onclick={() => filesInput.click()}
        >Choose files</Button
      >
    </div>
    <p>Keep this tab open until the import finishes.</p>
    {#if planning}<p role="status">Planning</p>{/if}
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
            <strong
              >{item.files.map((file) => file.webkitRelativePath || file.name).join(", ")}</strong
            ><span>{labels[item.state]}</span>
            {#if item.state === "hashing" || item.state === "uploading"}<progress
                max={item.progress.total || 1}
                value={item.progress.processed}
                aria-label={labels[item.state]}
              ></progress>{/if}
            {#if item.error}<p role="alert">{item.error}</p>{/if}
          </li>
        {/each}
        {#each unsupported as { file, reason }}<li>
            <strong>{file.webkitRelativePath || file.name}</strong><span>{reason}</span>
          </li>{/each}
      </ul>
      <div class="controls">
        <Button
          disabled={running || planning || !pending || !!disabledReason}
          onclick={() => void start()}>Add {pending} group{pending === 1 ? "" : "s"}</Button
        >
        {#if running}<Button disabled={stopping} onclick={() => (stopping = true)}
            >{stopping ? "Stopping after this group" : "Stop after this group"}</Button
          >{/if}
      </div>
    {/if}
  </div>
</DetailDrawer>

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
