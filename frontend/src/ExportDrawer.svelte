<script lang="ts">
  import { onDestroy, untrack } from "svelte";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, Card, Chip, CopyButton, DetailDrawer, IconButton, SelectDropdown, Spinner, TextInput } from "@kenn-io/kit-ui";
  import { APIError } from "./api.js";
  import { ExportSession, type ExportInput, type ExportState } from "./exportState.js";
  import { maxExportMembers, type RolePolicy } from "./exports.js";
  import { formatBytes, formatDate } from "./format.js";

  interface Props { session: string; open: boolean; input: ExportInput | null; onclose: () => void; onauthfailure: (error: unknown) => void; onactivechange?: (active: boolean) => void }
  let { session, open, input, onclose, onauthfailure, onactivechange }: Props = $props();
  let view = $state<Readonly<ExportState>>({ status: "idle" });
  let basename = $state("docbank-bundle.zip");
  let original = $state("required"), text = $state("omit"), pages = $state("omit");
  const controller = new ExportSession(untrack(() => session), next => view = next);
  let authFailureHandled = false;
  const options = [{ value: "omit", label: "Do not include" }, { value: "required", label: "Required — fail if unavailable" }, { value: "optional", label: "Optional — allow unavailable" }];
  const count = $derived(input ? ("members" in input ? input.members.length : input.snapshot.total) : 0);
  const busy = $derived(["preparing", "starting", "running"].includes(view.status));
  const admitted = $derived(view.active);
  const plan = $derived(admitted?.plan ?? view.reviewed?.plan);
  const receipt = $derived(admitted?.job?.receipt);
  const terminal = $derived(admitted?.job && ["completed", "failed", "canceled"].includes(admitted.job.state));
  $effect(() => { onactivechange?.(!!view.active); });

  $effect(() => {
    const current = input;
    const policies: RolePolicy[] = [["original", original], ["text", text], ["pages", pages]]
      .filter(([, choice]) => choice !== "omit")
      .map(([role, choice]) => ({ role: role!, ...(choice === "optional" ? { allow_unavailable: true } : {}) }));
    const name = basename;
    if (current) untrack(() => controller.choose(current, policies, name));
  });
  $effect(() => {
    if (!open) untrack(() => controller.close());
    else untrack(() => { if (view.active && view.status === "disconnected") void controller.reconnect(); });
  });
  $effect(() => {
    const error = view.error;
    if (error instanceof APIError && error.status === 401) untrack(() => {
      if (authFailureHandled) return;
      authFailureHandled = true;
      onauthfailure(error); close();
    });
  });
  onDestroy(() => controller.dispose());
  function close(): void { controller.close(); onclose(); }
</script>

{#if open}
  <DetailDrawer width="min(760px, 100vw)" ariaLabel="Verified export" onclose={close}>
    {#snippet header()}
      <div class="drawer-heading">
        <div><span>DOCUMENT EXPORT</span><strong>Preview and download</strong><small>Exact versions · verified ZIP archive</small></div>
        <IconButton size="sm" ariaLabel="Close export" onclick={close}><XIcon size="16" aria-hidden="true" /></IconButton>
      </div>
    {/snippet}
    <div class="exports">
      <section class="source" aria-label="Export source">
        <span>SOURCE</span><strong>{admitted?.label ?? input?.label ?? "No source selected"}</strong>
        <p>{(admitted?.plan.total ?? count).toLocaleString()} exact document{(admitted?.plan.total ?? count) === 1 ? "" : "s"}</p>
        {#if !admitted && input && "snapshot" in input}<p>All frozen pages are copied and checked before planning. Changes to the live query do not change this source.</p>{/if}
        {#if count > maxExportMembers}<p class="error">Exports are limited to 100,000 documents. Refine the query and capture a new snapshot.</p>{/if}
      </section>

      {#if !admitted}
        <section class="choices" aria-label="Export choices">
          <div class="role-choice"><strong>Original files</strong><SelectDropdown title="Original export policy" value={original} {options} onchange={value => original = value} /></div>
          <div class="role-choice"><strong>Retained text</strong><SelectDropdown title="Text export policy" value={text} {options} onchange={value => text = value} /></div>
          <div class="role-choice"><strong>Page images</strong><SelectDropdown title="Pages export policy" value={pages} {options} onchange={value => pages = value} /></div>
          <p>Required roles stop the plan if any document is unavailable. Optional roles record those omissions in the bundle.</p>
          <label for="export-basename">Downloaded ZIP filename</label>
          <TextInput id="export-basename" ariaLabel="Downloaded ZIP filename" bind:value={basename} block />
          <p>The filename applies to the download. Paths inside the bundle remain deterministic.</p>
          <Button tone="info" disabled={busy || count === 0 || count > maxExportMembers || [original, text, pages].every(value => value === "omit")} onclick={() => void controller.preview()}>
            {view.status === "preparing" ? "Copying source and freezing plan…" : "Preview export"}
          </Button>
        </section>
      {/if}

      <aside class="original-note">Originals are not redacted or sanitized by annotation overlays. Original file bytes remain unchanged.</aside>

      {#if view.error}<p role="alert" class="error">{view.error.message}</p>{/if}
      {#if view.status === "preparing"}<div role="status"><Spinner size={16} /> Checking exact membership and retained roles…</div>{/if}
      {#if view.status === "expired"}<p role="status">Export authority expired. A fresh preview is required before starting another export.</p>{/if}

      {#if plan}
        <Card level="default" padding="sm" title={admitted ? "Admitted export plan" : "Review this frozen plan"}>
          <dl>
            <div><dt>Documents</dt><dd data-testid="export-total">{plan.total.toLocaleString()}</dd></div>
            <div><dt>Role bytes</dt><dd>{formatBytes(plan.role_bytes)} · {plan.role_entries.toLocaleString()} files</dd></div>
            <div><dt>Download filename</dt><dd>{admitted?.basename ?? view.reviewed?.basename}</dd></div>
            <div><dt>Plan expires</dt><dd>{formatDate(plan.expires_at)}</dd></div>
          </dl>
          <p>Role bytes estimate payload only. The final ZIP size includes metadata and archive overhead.</p>
          <div class="identity"><span>MEMBER SHA-256</span><code data-testid="export-member-hash">{plan.source.member_hash}</code><CopyButton text={plan.source.member_hash} ariaLabel="Copy export member hash" /></div>
          <div class="identity"><span>PLAN FINGERPRINT</span><code data-testid="export-plan-fingerprint">{plan.fingerprint}</code><CopyButton text={plan.fingerprint} ariaLabel="Copy export plan fingerprint" /></div>
          {#if view.reviewed?.plan.id === plan.id}
            <div class="role-summaries">
              {#each view.reviewed.preview.roles as role (role.role)}
                <div class="role-summary"><strong>{role.role}</strong><Chip size="xs" tone={role.unavailable_members ? "warning" : "success"}>{role.available_members} available · {role.unavailable_members} unavailable</Chip><span>{role.files} files · {formatBytes(role.bytes)}</span>
                  {#if role.unavailable_reason}<p>{role.unavailable_reason}</p>{/if}
                </div>
              {/each}
            </div>
          {/if}
        </Card>
        {#if !admitted && view.reviewed}
          <Button tone="info" disabled={view.status !== "ready"} onclick={() => void controller.start()}>Start reviewed export</Button>
        {/if}
      {/if}

      {#if admitted}
        <section class="job" aria-label="Export job" aria-live="polite">
          <strong>{view.status === "completed" ? "Archive verified and ready" : view.status === "disconnected" ? "Disconnected — server work may continue" : view.status === "canceled" ? "Export canceled" : view.status === "failed" ? "Export failed" : view.status === "expired" ? "Export expired" : "Building verified archive"}</strong>
          {#if admitted.job}<p>{admitted.job.completed_roles.toLocaleString()} of {admitted.plan.role_entries.toLocaleString()} role files · {formatBytes(admitted.job.completed_bytes)} of {formatBytes(admitted.plan.role_bytes)} payload processed</p>{/if}
          {#if view.gap}<p>Progress advanced to a newer current-state snapshot. Intermediate updates were not replayed.</p>{/if}
          {#if admitted.job?.failure}<p class="error">{admitted.job.failure}</p>{/if}
          {#if receipt}
            <dl><div><dt>Final ZIP size</dt><dd data-testid="export-archive-size">{receipt.size} bytes</dd></div><div><dt>Archive entries</dt><dd>{receipt.entries.toLocaleString()}</dd></div></dl>
            <div class="identity"><span>ARCHIVE SHA-256</span><code data-testid="export-archive-hash">{receipt.sha256}</code><CopyButton text={receipt.sha256} ariaLabel="Copy archive hash" /></div>
            <Button tone="info" disabled={view.downloading || view.status === "expired"} onclick={() => void controller.download()}>{view.downloading ? "Reverifying download…" : "Download verified ZIP"}</Button>
            <p>{view.downloadOffered ? "Download handed to your browser. Check its download list for completion." : "Docbank verified the server archive. Your browser handles the download; its completion is separate."}</p>
          {/if}
          <div class="actions">
            {#if !terminal}<Button disabled={view.status === "starting"} onclick={() => void controller.reconnect()}>Reconnect to same export</Button><Button onclick={() => void controller.cancel()}>Cancel export</Button>{/if}
            {#if terminal}<Button onclick={() => controller.clearFinished()}>Prepare another export</Button>{/if}
          </div>
          <p>Closing this drawer stops progress readers. It does not cancel the server job. Reopen in this browser session to reconnect.</p>
        </section>
      {/if}
    </div>
  </DetailDrawer>
{/if}

<style>
  .drawer-heading { display: flex; align-items: center; justify-content: space-between; gap: var(--space-4); width: 100%; }
  .drawer-heading > div, .source, .exports, .choices, .job { display: grid; gap: var(--space-3); }
  .drawer-heading strong { font-size: var(--font-size-lg); }
  .drawer-heading span, .source > span, .identity > span { font-size: var(--font-size-xs); color: var(--text-muted); font-weight: var(--font-weight-bold); }
  .drawer-heading small { color: var(--text-muted); }
  .exports { padding: var(--space-5); gap: var(--space-5); }
  .source, .original-note, .job { padding: var(--space-4); border: 1px solid var(--border-muted); border-radius: var(--radius-lg); background: var(--bg-inset); }
  p { margin: 0; font-size: var(--font-size-sm); color: var(--text-secondary); }
  .role-choice { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
  .choices strong, label { font-size: var(--font-size-sm); }
  .original-note { color: var(--text-secondary); font-size: var(--font-size-sm); }
  .identity { display: flex; flex-wrap: wrap; gap: var(--space-2); align-items: center; margin-block: var(--space-3); }
  .identity > span { width: 100%; }
  code { font-size: var(--font-size-xs); overflow-wrap: anywhere; min-width: 0; }
  dl { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-3); margin: 0 0 var(--space-3); }
  dt { font-size: var(--font-size-xs); color: var(--text-muted); }
  dd { margin: var(--space-1) 0 0; overflow-wrap: anywhere; font-size: var(--font-size-sm); }
  .role-summaries { display: grid; gap: var(--space-3); }
  .role-summary { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); font-size: var(--font-size-sm); }
  .role-summary p { width: 100%; }
  .actions { display: flex; flex-wrap: wrap; gap: var(--space-3); }
  .error { color: var(--accent-red); }
  @media (max-width: 640px) { .role-choice { display: grid; } dl { grid-template-columns: 1fr; } }
</style>
