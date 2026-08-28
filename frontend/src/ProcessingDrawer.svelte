<script lang="ts">
  import { onMount } from "svelte";
  import ActivityIcon from "@lucide/svelte/icons/activity";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import SearchIcon from "@lucide/svelte/icons/search";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, Card, Chip, DetailDrawer, IconButton, SearchInput, Spinner } from "@kenn-io/kit-ui";
  import {
    APIError,
    documentCoverage,
    documentSearch,
    processingPlan,
    processingProfiles,
    startProcessing,
    type CoverageReport,
    type DocumentSearchReport,
    type Node,
    type ProcessingPlan,
    type ProcessingProfileSummary,
    type ProcessingRun,
  } from "./api.js";

  interface Props {
    session: string;
    node: Node;
    path: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
    onrendition: (attachmentID: string) => void;
  }

  let { session, node, path, onclose, onauthfailure, onrendition }: Props = $props();
  let profiles = $state<ProcessingProfileSummary[]>([]);
  let profileName = $state("");
  let plan = $state<ProcessingPlan | null>(null);
  let coverage = $state<CoverageReport | null>(null);
  let run = $state<ProcessingRun | null>(null);
  let loading = $state(true);
  let running = $state(false);
  let error = $state("");
  let query = $state("");
  let searching = $state(false);
  let searchReport = $state<DocumentSearchReport | null>(null);
  let generation = 0;

  onMount(() => {
    void loadProfiles();
    return () => { generation += 1; };
  });

  function handleFailure(cause: unknown): void {
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      onclose();
      return;
    }
    error = cause instanceof Error ? cause.message : String(cause);
  }

  async function loadProfiles(): Promise<void> {
    const request = ++generation;
    loading = true;
    error = "";
    try {
      profiles = await processingProfiles(session);
      if (request !== generation) return;
      profileName = profiles[0]?.name ?? "";
      if (profileName) await preview(request);
    } catch (cause) {
      if (request === generation) handleFailure(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  async function preview(existingRequest?: number): Promise<void> {
    if (!node.current_version_id || !profileName) return;
    const request = existingRequest ?? ++generation;
    loading = true;
    error = "";
    run = null;
    searchReport = null;
    try {
      const next = await processingPlan(session, {
        node_id: node.id,
        content_version_id: node.current_version_id,
        profile: profileName,
      });
      if (request !== generation) return;
      plan = next;
      coverage = await documentCoverage(session, profileName, next.vault_uid, [node.current_version_id]);
    } catch (cause) {
      if (request === generation) handleFailure(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  async function execute(): Promise<void> {
    if (!plan || !node.current_version_id) return;
    running = true;
    error = "";
    try {
      run = await startProcessing(session, plan.selector, plan.fingerprint, plan.consent_required);
      coverage = await documentCoverage(session, profileName, plan.vault_uid, [node.current_version_id]);
    } catch (cause) {
      handleFailure(cause);
    } finally {
      running = false;
    }
  }

  async function searchVersion(): Promise<void> {
    if (!plan || !node.current_version_id || !query.trim()) return;
    searching = true;
    error = "";
    try {
      searchReport = await documentSearch(session, {
        query: query.trim(), mode: "auto", limit: 20, profile: profileName,
        binding_id: profiles.find((item) => item.name === profileName)?.embedding_bindings[0],
        fence: { vault_uid: plan.vault_uid, content_version_ids: [node.current_version_id] },
        explain: true,
      });
    } catch (cause) {
      handleFailure(cause);
    } finally {
      searching = false;
    }
  }

  function boundaryLabel(boundary: string): string {
    switch (boundary) {
      case "local_process": return "Local process";
      case "operator_network": return "Private network";
      case "hosted_provider": return "Hosted provider";
      default: return "Unrecognized boundary";
    }
  }

  function boundaryDetail(boundary: string): string {
    switch (boundary) {
      case "local_process": return "Processing stays inside this Docbank process.";
      case "operator_network": return "Document data goes to an operator-controlled private endpoint.";
      case "hosted_provider": return "Document data leaves this machine for this step.";
      default: return "Treat this step as external until its trust boundary is configured correctly.";
    }
  }

  function consentLabel(state: ProcessingPlan["consent_state"]): string {
    switch (state) {
      case "active": return "Consent active";
      case "required": return "Consent required";
      case "expired": return "Consent expired";
      case "revoked": return "Consent revoked";
    }
  }
</script>

<DetailDrawer width="min(900px, 100vw)" ariaLabel="Document processing and coverage" {onclose}>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>DOCUMENT PROCESSING</span>
        <strong>{path}</strong>
        <small>Exact version {node.current_version_id}</small>
      </div>
      <div class="drawer-actions">
        <IconButton size="sm" ariaLabel="Refresh processing plan" disabled={loading || running} onclick={() => void preview()}>
          <RefreshCwIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton size="sm" ariaLabel="Close document processing" onclick={onclose}>
          <XIcon size="14" aria-hidden="true" />
        </IconButton>
      </div>
    </div>
  {/snippet}

  <div class="processing-shell">
    {#if loading && !plan}
      <div class="loading"><Spinner size={16} /> Preparing the exact disclosure plan…</div>
    {:else if error && !plan}
      <div class="load-error"><p role="alert">{error}</p><Button size="sm" onclick={() => void loadProfiles()}>Try again</Button></div>
    {:else if profiles.length === 0}
      <Card level="default" title="No executable processing profiles" eyebrow="NOT CONFIGURED">
        <p>The daemon will not advertise a provider flow it cannot execute end to end.</p>
      </Card>
    {:else if plan}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <div class="profile-row">
        <label for="processing-profile">Profile</label>
        <select id="processing-profile" bind:value={profileName} onchange={() => void preview()} disabled={loading || running}>
          {#each profiles as profile}<option value={profile.name}>{profile.name}</option>{/each}
        </select>
      </div>

      <section aria-label="Reviewed provider flow">
        <div class="section-heading"><div><span>REVIEWED FLOW</span><strong>What leaves the vault, and what stays</strong></div><Chip size="xs" tone={plan.consent_state === "active" ? "success" : "warning"}>{consentLabel(plan.consent_state)}</Chip></div>
        <div class="flow-list">
          {#each plan.flow as hop}
            <Card level="default" padding="sm" eyebrow={hop.capability.toUpperCase()} title={hop.provider_id}>
              {#snippet actions()}<Chip size="xs" tone={hop.trust_boundary === "local_process" || hop.trust_boundary === "operator_network" ? "success" : "warning"}>{boundaryLabel(hop.trust_boundary)}</Chip>{/snippet}
              <p>{hop.input_classes.join(", ")}</p>
              <small>{boundaryDetail(hop.trust_boundary)}</small>
            </Card>
          {/each}
        </div>
        {#if plan.retained_classes.includes("sanitized_markdown")}
          <p class="retention-warning">Retained sanitized Markdown becomes durable Docbank authority and is included in future backups.</p>
        {/if}
        <p class="consent-copy">Consent covers this processing profile and operator scope until revoked or expired. The exact plan fingerprint is still reviewed before each run.</p>
        <Button size="sm" tone="info" disabled={running || loading} onclick={() => void execute()}>
          {#if running}<Spinner size={14} /> Running…{:else}<ActivityIcon size="14" aria-hidden="true" /> {plan.consent_required ? "Consent and run" : "Run processing"}{/if}
        </Button>
      </section>

      <section aria-label="Reviewed processing scope">
        <div class="section-heading"><div><span>REVIEWED SCOPE</span><strong>Exact plan and retained authority</strong></div></div>
        <div class="fingerprints">
          <div><span>Plan fingerprint</span><code>{plan.fingerprint}</code></div>
          <div><span>Profile fingerprint</span><code>{plan.profile_fingerprint}</code></div>
        </div>
        <p><strong>Disclosed:</strong> {plan.disclosed_classes.join(", ") || "none"}</p>
        <p><strong>Retained:</strong> {plan.retained_classes.join(", ") || "none"}</p>
        <p>{plan.estimate.source_bytes} source bytes · {plan.estimate.provider_calls} provider call{plan.estimate.provider_calls === 1 ? "" : "s"} · {plan.estimate.vector_spaces} vector space{plan.estimate.vector_spaces === 1 ? "" : "s"}</p>
        <p><strong>Backup consequence:</strong> {plan.backup_consequence}</p>
      </section>

      {#if run}
        <section aria-live="polite">
          <div class="section-heading"><div><span>DURABLE JOB</span><strong>{run.status.state}</strong></div><Chip size="xs" tone={run.status.state === "complete" ? "success" : "warning"}>{run.status.phase}</Chip></div>
          <code>{run.job.id}</code>
          {#if run.status.failure_code}<p class="warning">{run.status.failure_code}</p>{/if}
          {#if run.job.attachment_id}<Button size="sm" surface="soft" onclick={() => onrendition(run!.job.attachment_id!)}>Read sanitized Markdown</Button>{/if}
        </section>
      {/if}

      {#if coverage}
        <section aria-label="Document processing coverage">
          <div class="section-heading"><div><span>COVERAGE</span><strong>{coverage.state}</strong></div></div>
          {#if coverage.state === "rebuilding"}<p>Previous complete generation remains available while the rebuild runs.</p>{/if}
          <div class="coverage-list">
            <div><strong>Rendition · {coverage.renditions.state}</strong><span>{coverage.renditions.complete}/{coverage.renditions.total} complete</span></div>
            {#each coverage.embeddings as item}<div><strong>{item.name} · {item.state}</strong><span>{item.required ? "Required" : "Optional"} · {item.complete}/{item.total} complete</span></div>{/each}
          </div>
        </section>
      {/if}

      <section aria-label="Search exact document version">
        <div class="section-heading"><div><span>RETRIEVAL PROOF</span><strong>Search this exact version</strong></div></div>
        <form class="document-search" onsubmit={(event) => { event.preventDefault(); void searchVersion(); }}>
          <SearchInput bind:value={query} ariaLabel="Search this document version" placeholder="Search retained evidence" block />
          <Button type="submit" size="sm" disabled={searching || !query.trim()}><SearchIcon size="14" aria-hidden="true" />Search this version</Button>
        </form>
        {#if searchReport}
          <p>Actual mode: <strong>{searchReport.actual_mode}</strong> · coverage {searchReport.coverage.state}</p>
          {#each searchReport.degradations as degradation}<p class="warning">{degradation}</p>{/each}
          {#each searchReport.results as result}
            <Card level="default" padding="sm" eyebrow={`RANK ${result.rank}`} title={result.path}>
              <p>{result.excerpt ?? "Direct-file result; no text excerpt."}</p>
              <small>{result.evidence.map((item) => item.kind).join(", ")}</small>
            </Card>
          {/each}
        {/if}
      </section>
    {/if}
  </div>
</DetailDrawer>

<style>
  .drawer-heading, .section-heading, .profile-row, .document-search { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
  .drawer-heading { width: 100%; }
  .drawer-heading > div:first-child, .section-heading > div { display: grid; gap: var(--space-1); min-width: 0; }
  .drawer-heading span, .section-heading span { color: var(--text-muted); font-size: var(--font-size-xs); font-weight: var(--font-weight-bold); letter-spacing: .04em; }
  .drawer-heading strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: var(--font-size-lg); }
  .drawer-heading small, section small { color: var(--text-muted); font-size: var(--font-size-xs); }
  .drawer-actions { display: flex; gap: var(--space-2); }
  .processing-shell { display: grid; gap: var(--space-5); padding: var(--space-5); }
  section { display: grid; gap: var(--space-3); padding-top: var(--space-4); border-top: 1px solid var(--border-subtle); }
  section p, section code, .flow-list p { margin: 0; color: var(--text-secondary); font-size: var(--font-size-sm); }
  .loading { display: flex; align-items: center; gap: var(--space-3); color: var(--text-secondary); }
  .load-error { display: grid; justify-items: start; gap: var(--space-3); }
  .error, .warning, .load-error p { color: var(--accent-red); }
  .profile-row { justify-content: flex-start; }
  .profile-row label { color: var(--text-muted); font-size: var(--font-size-xs); font-weight: var(--font-weight-bold); }
  select { min-height: 36px; padding: 0 var(--space-3); border: 1px solid var(--border-subtle); border-radius: var(--radius-md); background: var(--surface-raised); color: var(--text-primary); }
  .flow-list { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: var(--space-3); }
  .retention-warning { padding: var(--space-3); border-left: 3px solid var(--accent-amber); background: color-mix(in srgb, var(--accent-amber) 8%, transparent); }
  .consent-copy { color: var(--text-muted); }
  .coverage-list { display: grid; gap: var(--space-2); }
  .coverage-list div { display: flex; justify-content: space-between; gap: var(--space-3); padding: var(--space-3); border: 1px solid var(--border-subtle); border-radius: var(--radius-md); }
  .coverage-list span { color: var(--text-muted); font-size: var(--font-size-xs); }
  .document-search :global(.kit-search-input) { flex: 1; }
  .fingerprints { display: grid; gap: var(--space-2); }
  .fingerprints div { display: grid; gap: var(--space-1); }
  .fingerprints span { color: var(--text-muted); font-size: var(--font-size-xs); font-weight: var(--font-weight-bold); text-transform: uppercase; }
  code { overflow-wrap: anywhere; }
  @media (max-width: 640px) { .profile-row, .document-search, .coverage-list div { align-items: stretch; flex-direction: column; } }
</style>
