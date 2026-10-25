<script lang="ts">
  import { onDestroy } from "svelte";
  import FileArchiveIcon from "@lucide/svelte/icons/file-archive";
  import { Button, Checkbox, Chip, SelectDropdown, Spinner, TextInput } from "@kenn-io/kit-ui";
  import DrawerShell from "./DrawerShell.svelte";
  import FileChooser from "./FileChooser.svelte";
  import "./toolForm.css";
  import { APIError } from "./api-transport.js";
  import { randomUUID } from "./crypto.js";
  import { formatBytes } from "./format.js";
  import type { PackageImportJob, PackagePreflight } from "./generated/docbank.js";
  import {
    preflightPackageZIP,
    readPackageImport,
    startPackageImport,
    uploadPackageZIP,
    type PackageChannel,
  } from "./loadfile.js";

  interface Props {
    session: string;
    channel: PackageChannel;
    destination: string;
    onclose: () => void;
    oncomplete: () => void | Promise<void>;
    onauthfailure: (cause: unknown) => void;
  }
  let { session, channel, destination, onclose, oncomplete, onauthfailure }: Props = $props();
  let file = $state<File | null>(null),
    profile = $state("dat-concordance-v1"),
    pageMapProfile = $state(""),
    encoding = $state("utf-8");
  let name = $state(""),
    party = $state(""),
    acceptPartial = $state(false),
    indexText = $state(true);
  let busy = $state(false),
    error = $state(""),
    stage = $state(""),
    processed = $state(0),
    total = $state(0);
  let containerID = "",
    operationID = "";
  let sourceSealed = false;
  let preview = $state<PackagePreflight | null>(null),
    job = $state<PackageImportJob | null>(null);
  let controller = new AbortController(),
    poll: ReturnType<typeof setInterval> | undefined;
  onDestroy(() => {
    controller.abort();
    if (poll) clearInterval(poll);
  });
  function fail(cause: unknown): void {
    if (cause instanceof DOMException && cause.name === "AbortError") return;
    error = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
  }
  function choose(event: Event): void {
    file = (event.currentTarget as HTMLInputElement).files?.[0] ?? null;
    name =
      file?.name
        .replace(/\.zip$/i, "")
        .replace(/[^A-Za-z0-9._-]+/g, "-")
        .replace(/^-+|-+$/g, "") || "package";
    preview = null;
    job = null;
    error = "";
    containerID = "";
    operationID = "";
    sourceSealed = false;
  }
  async function previewPackage(): Promise<void> {
    if (!file || busy) return;
    busy = true;
    error = "";
    controller = new AbortController();
    if (!containerID) containerID = randomUUID();
    try {
      if (!sourceSealed) {
        await uploadPackageZIP(
          session,
          channel,
          file,
          containerID,
          controller.signal,
          (label, progress) => {
            stage = label;
            processed = progress.processed;
            total = progress.total;
          },
        );
        sourceSealed = true;
      }
      stage = "Checking package";
      preview = await preflightPackageZIP(
        session,
        containerID,
        profile,
        pageMapProfile,
        encoding,
        controller.signal,
      );
      stage = "Preview ready";
    } catch (cause) {
      fail(cause);
    } finally {
      busy = false;
    }
  }
  async function start(): Promise<void> {
    if (!preview || busy) return;
    busy = true;
    error = "";
    if (!operationID) operationID = randomUUID();
    try {
      job = await startPackageImport(
        session,
        {
          preflight_id: preview.preflight_id,
          into: destination,
          name,
          party,
          operation_id: operationID,
          accept_partial: acceptPartial,
          index_supplied_text: indexText,
        },
        controller.signal,
      );
      preview = null;
      poll = setInterval(() => void refresh(), 1000);
    } catch (cause) {
      fail(cause);
    } finally {
      busy = false;
    }
  }
  async function refresh(): Promise<void> {
    if (!operationID || controller.signal.aborted) return;
    try {
      const previous = job?.state;
      job = await readPackageImport(session, operationID, controller.signal);
      if (!["queued", "running"].includes(job.state)) {
        if (poll) clearInterval(poll);
        poll = undefined;
        if (
          ["complete", "partial"].includes(job.state) &&
          ["queued", "running"].includes(previous ?? "")
        )
          await oncomplete();
      }
    } catch (cause) {
      fail(cause);
    }
  }
</script>

<DrawerShell
  title={`Import load files to ${destination}`}
  ariaLabel="Import load-file package"
  closeLabel="Close load-file import"
  {onclose}
>
  <div class="tool-body">
    <section class="tool-section" aria-label="Package source">
      <FileChooser
        accept=".zip,application/zip"
        label="Choose load-file ZIP"
        {file}
        disabled={busy}
        icon={FileArchiveIcon}
        onchange={choose}
      />
      <div class="tool-grid">
        <div class="tool-field tool-span-2">
          <span class="tool-label">Metadata</span>
          <SelectDropdown
            title="Metadata profile"
            value={profile}
            options={[
              { value: "dat-concordance-v1", label: "Concordance DAT" },
              { value: "csv-rfc4180-v1", label: "RFC 4180 CSV" },
            ]}
            disabled={busy || Boolean(preview)}
            onchange={(value) => {
              profile = value;
            }}
          />
        </div>
        <div class="tool-field tool-span-2">
          <span class="tool-label">Page map</span>
          <SelectDropdown
            title="Page map profile"
            value={pageMapProfile}
            options={[
              { value: "", label: "Detect from extension" },
              { value: "opt-standard-v1", label: "Standard OPT" },
              { value: "opt-pagecount5-v1", label: "OPT page count" },
              { value: "lfp-ipro-v1", label: "IPRO LFP" },
            ]}
            disabled={busy || Boolean(preview)}
            onchange={(value) => {
              pageMapProfile = value;
            }}
          />
        </div>
        <div class="tool-field tool-span-2">
          <span class="tool-label">Encoding</span>
          <SelectDropdown
            title="Source encoding"
            value={encoding}
            options={[
              { value: "utf-8", label: "UTF-8" },
              { value: "windows-1252", label: "Windows-1252" },
            ]}
            disabled={busy || Boolean(preview)}
            onchange={(value) => {
              encoding = value;
            }}
          />
        </div>
      </div>
      {#if error}<p role="alert" class="tool-error">{error}</p>{/if}
      {#if !preview && !job}
        <div class="tool-actions">
          {#if busy}<span class="tool-muted" role="status"
              ><Spinner size={14} />
              {stage}{#if total}
                · {formatBytes(processed)} of {formatBytes(total)}{/if}</span
            >{/if}
          <Button
            tone="info"
            surface="solid"
            disabled={busy || !file}
            onclick={() => void previewPackage()}>Upload and preview</Button
          >
        </div>
      {/if}
    </section>

    {#if preview}
      {@const currentPreview = preview}
      <section class="tool-section" aria-label="Package preview">
        <div class="tool-heading">
          <h3>Package preview</h3>
          <Chip size="xs" tone={currentPreview.blocking ? "danger" : "success"}
            >{currentPreview.blocking ? "Blocked" : "Ready"}</Chip
          >
        </div>
        <dl class="tool-stats">
          <div>
            <dt>Records</dt>
            <dd>{currentPreview.records}</dd>
          </div>
          <div>
            <dt>Pages</dt>
            <dd>{currentPreview.pages}</dd>
          </div>
          <div>
            <dt>Diagnostics</dt>
            <dd>{currentPreview.diagnostic_count}</dd>
          </div>
        </dl>
        {#if currentPreview.diagnostics.length}
          <ul class="tool-list">
            {#each currentPreview.diagnostics as diagnostic}<li
                class:tool-error={diagnostic.severity === "blocking"}
              >
                <span><strong>{diagnostic.code}</strong> {diagnostic.detail}</span>
              </li>{/each}
          </ul>
        {/if}
        {#if !job}
          <div class="tool-grid">
            <div class="tool-field tool-span-3">
              <label for="loadfile-name">Package name</label>
              <TextInput id="loadfile-name" bind:value={name} block />
            </div>
            <div class="tool-field tool-span-3">
              <label for="loadfile-party">Sending party</label>
              <TextInput id="loadfile-party" bind:value={party} block />
            </div>
            <div class="checks tool-span-all">
              <Checkbox
                label="Index package-supplied text"
                checked={indexText}
                onchange={(checked) => {
                  indexText = checked;
                }}
              /><Checkbox
                label="Keep supported records and report gaps"
                checked={acceptPartial}
                onchange={(checked) => {
                  acceptPartial = checked;
                }}
              />
            </div>
          </div>
          {#if name.length > 128 || party.length > 64}<p class="tool-error">
              Use at most 128 characters for the name and 64 for the sending party.
            </p>{/if}
          <div class="tool-actions">
            {#if busy}<span class="tool-muted" role="status"><Spinner size={14} /> {stage}</span
              >{/if}
            <Button
              tone="info"
              surface="solid"
              disabled={busy ||
                currentPreview.blocking ||
                !name ||
                name.length > 128 ||
                party.length > 64}
              onclick={() => void start()}>Import package</Button
            >
          </div>
        {/if}
      </section>
    {/if}
    {#if job}{@const currentJob = job}
      <section class="tool-section" aria-label="Import report">
        <div class="tool-heading">
          <h3>{currentJob.state === "complete" ? "Import complete" : "Package import"}</h3>
          <Chip
            size="xs"
            tone={currentJob.state === "complete"
              ? "success"
              : currentJob.state === "failed"
                ? "danger"
                : "info"}>{currentJob.state}</Chip
          >
        </div>
        <dl class="tool-stats">
          <div>
            <dt>Imported</dt>
            <dd>{currentJob.committed}</dd>
          </div>
          <div>
            <dt>Gaps</dt>
            <dd>{currentJob.gap_count}</dd>
          </div>
          <div>
            <dt>Total</dt>
            <dd>{currentJob.total}</dd>
          </div>
        </dl>
        <p class="tool-muted">Operation <code>{currentJob.operation_id}</code></p>
      </section>
    {/if}
  </div>
</DrawerShell>

<style>
  .checks {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-7);
  }
  code {
    font-size: var(--font-size-sm);
    overflow-wrap: anywhere;
  }
</style>
