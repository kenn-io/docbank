<script lang="ts">
  import { onDestroy } from "svelte";
  import XIcon from "@lucide/svelte/icons/x";
  import FileArchiveIcon from "@lucide/svelte/icons/file-archive";
  import { Button, Card, Checkbox, Chip, DetailDrawer, IconButton, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import { formatBytes } from "./format.js";
  import type { PackageImportJob, PackagePreflight } from "./generated/docbank.js";
  import { preflightPackageZIP, readPackageImport, startPackageImport, uploadPackageZIP, type PackageChannel } from "./loadfile.js";

  interface Props {
    session: string;
    channel: PackageChannel;
    destination: string;
    onclose: () => void;
    oncomplete: () => void | Promise<void>;
    onauthfailure: (cause: unknown) => void;
  }
  let { session, channel, destination, onclose, oncomplete, onauthfailure }: Props = $props();
  let file = $state<File | null>(null), profile = $state("dat-concordance-v1"), pageMapProfile = $state(""), encoding = $state("utf-8");
  let name = $state(""), party = $state(""), acceptPartial = $state(false), indexText = $state(true);
  let busy = $state(false), error = $state(""), stage = $state(""), processed = $state(0), total = $state(0);
  let containerID = "", operationID = "";
  let preview = $state<PackagePreflight | null>(null), job = $state<PackageImportJob | null>(null);
  let controller = new AbortController(), poll: ReturnType<typeof setInterval> | undefined;
  onDestroy(() => { controller.abort(); if (poll) clearInterval(poll); });
  function fail(cause: unknown): void {
    if (cause instanceof DOMException && cause.name === "AbortError") return;
    error = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
  }
  function choose(event: Event): void {
    file = (event.currentTarget as HTMLInputElement).files?.[0] ?? null;
    name = file?.name.replace(/\.zip$/i, "").replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "") || "package";
    preview = null; job = null; error = ""; containerID = ""; operationID = "";
  }
  async function previewPackage(): Promise<void> {
    if (!file || busy) return;
    busy = true; error = ""; controller = new AbortController();
    if (!containerID) containerID = crypto.randomUUID();
    try {
      await uploadPackageZIP(session, channel, file, containerID, controller.signal, (label, progress) => {
        stage = label; processed = progress.processed; total = progress.total;
      });
      stage = "Checking package";
      preview = await preflightPackageZIP(session, containerID, profile, pageMapProfile, encoding, controller.signal);
      stage = "Preview ready";
    } catch (cause) { fail(cause); }
    finally { busy = false; }
  }
  async function start(): Promise<void> {
    if (!preview || busy) return;
    busy = true; error = "";
    if (!operationID) operationID = crypto.randomUUID();
    try {
      job = await startPackageImport(session, {
        preflight_id: preview.preflight_id, into: destination, name, party,
        operation_id: operationID, accept_partial: acceptPartial, index_supplied_text: indexText,
      }, controller.signal);
      preview = null;
      poll = setInterval(() => void refresh(), 1000);
    } catch (cause) { fail(cause); }
    finally { busy = false; }
  }
  async function refresh(): Promise<void> {
    if (!operationID || controller.signal.aborted) return;
    try {
      const previous = job?.state;
      job = await readPackageImport(session, operationID, controller.signal);
      if (!["queued", "running"].includes(job.state)) {
        if (poll) clearInterval(poll); poll = undefined;
        if (["complete", "partial"].includes(job.state) && ["queued", "running"].includes(previous ?? "")) await oncomplete();
      }
    } catch (cause) { fail(cause); }
  }
</script>

<DetailDrawer width="min(720px, 100vw)" ariaLabel="Import load-file package" {onclose}>
  {#snippet header()}<div class="drawer-heading"><div><span>LOAD-FILE IMPORT</span><strong>Import reviewed package</strong><small>{destination}</small></div><IconButton size="sm" ariaLabel="Close load-file import" onclick={onclose}><XIcon size="14" /></IconButton></div>{/snippet}
  <div class="content">
    <p>Upload a bounded ZIP, review its records, pages, and diagnostics, then start a resumable import.</p>
    <label class="file-label">Choose load-file ZIP<input type="file" accept=".zip,application/zip" aria-label="Choose load-file ZIP" disabled={busy} onchange={choose} /></label>
    <div class="choices"><label>Metadata profile<SelectDropdown title="Metadata profile" value={profile} options={[{value:"dat-concordance-v1",label:"Concordance DAT"},{value:"csv-rfc4180-v1",label:"RFC 4180 CSV"}]} disabled={busy||Boolean(preview)} onchange={(value)=>{profile=value;}} /></label><label>Page map<SelectDropdown title="Page map profile" value={pageMapProfile} options={[{value:"",label:"Detect from extension"},{value:"opt-standard-v1",label:"Standard OPT"},{value:"opt-pagecount5-v1",label:"OPT page count"},{value:"lfp-ipro-v1",label:"IPRO LFP"}]} disabled={busy||Boolean(preview)} onchange={(value)=>{pageMapProfile=value;}} /></label><label>Encoding<SelectDropdown title="Source encoding" value={encoding} options={[{value:"utf-8",label:"UTF-8"},{value:"windows-1252",label:"Windows-1252"}]} disabled={busy||Boolean(preview)} onchange={(value)=>{encoding=value;}} /></label></div>
    {#if file}<div class="file-name"><FileArchiveIcon size="18" /><strong>{file.name}</strong><span>{formatBytes(file.size)}</span></div><Button tone="info" disabled={busy} onclick={()=>void previewPackage()}>Upload and preview</Button>{/if}
    {#if busy}<div class="progress"><Spinner size={16}/><span>{stage}{#if total} · {formatBytes(processed)} / {formatBytes(total)}{/if}</span></div>{/if}
    {#if error}<p role="alert" class="error">{error}</p>{/if}
    {#if preview}
      {@const currentPreview = preview}
      <Card title="Package preview" eyebrow="SOURCE VERIFIED" padding="md">
        {#snippet actions()}<Chip size="xs" tone={currentPreview.blocking?"danger":"success"}>{currentPreview.blocking?"Blocked":"Ready"}</Chip>{/snippet}
        <div class="counts"><div><strong>{currentPreview.records}</strong><span>Records</span></div><div><strong>{currentPreview.pages}</strong><span>Pages</span></div><div><strong>{currentPreview.diagnostic_count}</strong><span>Diagnostics</span></div></div>
        {#each currentPreview.diagnostics as diagnostic}<p class:diagnostic-error={diagnostic.severity==="blocking"}><strong>{diagnostic.code}</strong> {diagnostic.detail}</p>{/each}
        <label>Package name<input bind:value={name} maxlength="128" /></label><label>Sending party<input bind:value={party} maxlength="64" /></label>
        <Checkbox label="Index package-supplied text" checked={indexText} onchange={(checked)=>{indexText=checked;}} /><Checkbox label="Keep supported records and report gaps" checked={acceptPartial} onchange={(checked)=>{acceptPartial=checked;}} />
        <Button tone="success" disabled={busy||currentPreview.blocking||!name} onclick={()=>void start()}>Import package</Button>
      </Card>
    {/if}
    {#if job}{@const currentJob = job}<Card title={currentJob.state==="complete"?"Import complete":"Package import"} eyebrow="DURABLE IMPORT REPORT" padding="md">{#snippet actions()}<Chip size="xs" tone={currentJob.state==="complete"?"success":currentJob.state==="failed"?"danger":"info"}>{currentJob.state}</Chip>{/snippet}<div class="counts"><div><strong>{currentJob.committed}</strong><span>Imported</span></div><div><strong>{currentJob.gap_count}</strong><span>Gaps</span></div><div><strong>{currentJob.total}</strong><span>Total</span></div></div><small>Operation {currentJob.operation_id}</small></Card>{/if}
  </div>
</DetailDrawer>

<style>
  .drawer-heading,.file-name,.progress{display:flex;align-items:center;gap:12px}.drawer-heading{justify-content:space-between;width:100%}.drawer-heading>div{display:flex;flex-direction:column;min-width:0}.drawer-heading span,small{color:var(--text-muted);font-size:12px}.drawer-heading strong{font-size:var(--font-size-lg)}.content{display:grid;gap:16px;padding:24px;overflow:auto}.content p{margin:0;line-height:1.5}.file-label,.choices label,.content>label{display:grid;gap:8px;font-size:13px}.choices{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:12px}.file-name{padding:12px;border:1px solid var(--border);border-radius:8px}.file-name strong{flex:1}.counts{display:flex;gap:24px;margin:12px 0}.counts div{display:grid;gap:4px}.counts strong{font-size:26px}.counts span{color:var(--text-secondary);font-size:12px}.error,.diagnostic-error{color:var(--danger-text)}input{padding:9px;border:1px solid var(--border);border-radius:6px;background:var(--surface);color:var(--text-primary)}
</style>
