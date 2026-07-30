<script lang="ts">
  import { onMount } from "svelte";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import ShieldCheckIcon from "@lucide/svelte/icons/shield-check";
  import TriangleAlertIcon from "@lucide/svelte/icons/triangle-alert";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Card,
    Chip,
    CopyButton,
    DetailDrawer,
    EmptyState,
    IconButton,
    Spinner,
  } from "@kenn-io/kit-ui";
  import {
    APIError,
    verifyAudit,
    type AuditVerifyReport,
  } from "./api.js";
  import { formatBytes } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onauthfailure }: Props = $props();

  let report = $state<AuditVerifyReport | null>(null);
  let loading = $state(true);
  let error = $state("");
  let controller: AbortController | null = null;
  let generation = 0;

  const metadataProblems = $derived(report?.metadata_problems ?? []);
  const contentProblems = $derived(report?.problems ?? []);
  const evidence = $derived(report?.evidence);
  const visibleScopes = $derived(evidence?.scopes.slice(0, 100) ?? []);
  const verified = $derived(
    Boolean(
      report?.enabled &&
        evidence &&
        metadataProblems.length === 0 &&
        contentProblems.length === 0 &&
        report.verified_blobs === report.protected_blobs,
    ),
  );

  onMount(() => {
    void refresh();
    return () => {
      generation += 1;
      controller?.abort();
    };
  });

  async function refresh(): Promise<void> {
    controller?.abort();
    const active = new AbortController();
    controller = active;
    const request = ++generation;
    loading = true;
    error = "";
    try {
      const next = await verifyAudit(session, active.signal);
      if (request !== generation) return;
      report = next;
    } catch (cause) {
      if (request !== generation) return;
      if (cause instanceof DOMException && cause.name === "AbortError") return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        onclose();
        return;
      }
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === generation) {
        loading = false;
        controller = null;
      }
    }
  }
</script>

<DetailDrawer
  width="min(860px, 100vw)"
  ariaLabel="Permanent audit verification"
  {onclose}
>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>PERMANENT AUDIT</span>
        <strong>Independent verification</strong>
        <small>
          {#if report?.enabled && evidence}
            {evidence.scopes.length} protected scope{evidence.scopes.length === 1 ? "" : "s"}
            · {report.verified_blobs} of {report.protected_blobs} blobs verified
          {:else}
            Replay history and prove protected bytes
          {/if}
        </small>
      </div>
      <div class="drawer-actions">
        <IconButton
          size="sm"
          ariaLabel="Run permanent audit verification again"
          disabled={loading}
          onclick={() => void refresh()}
        >
          <RefreshCwIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton size="sm" ariaLabel="Close permanent audit verification" onclick={onclose}>
          <XIcon size="14" aria-hidden="true" />
        </IconButton>
      </div>
    </div>
  {/snippet}

  <div class="verification">
    {#if loading && !report}
      <div class="loading">
        <Spinner size={16} />
        Replaying protected history and hashing retained content…
      </div>
    {:else if error && !report}
      <div class="load-error">
        <p role="alert">{error}</p>
        <Button size="sm" onclick={() => void refresh()}>Try again</Button>
      </div>
    {:else if report && !report.enabled}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <EmptyState
        title="Permanent audit is dormant"
        description="No directory has been enrolled in permanent audited history, so there is no protected chain or retained byte set to verify."
      >
        {#snippet icon()}<ShieldCheckIcon size="22" />{/snippet}
      </EmptyState>
    {:else if report && evidence}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <section class="result-heading" aria-live="polite">
        <div class:failed={!verified}>
          {#if verified}
            <ShieldCheckIcon size="24" aria-hidden="true" />
          {:else}
            <TriangleAlertIcon size="24" aria-hidden="true" />
          {/if}
          <div>
            <span>{verified ? "VERIFIED" : "PROBLEMS FOUND"}</span>
            <strong>
              {verified
                ? "Protected history and content agree"
                : "Protected authority needs attention"}
            </strong>
          </div>
        </div>
        <Chip size="sm" tone={verified ? "success" : "danger"} dot>
          {verified ? "Complete" : "Failed"}
        </Chip>
      </section>

      <div class="summary-grid">
        <Card
          level="default"
          padding="sm"
          eyebrow="PROTECTED CONTENT"
          title={formatBytes(report.protected_bytes)}
        >
          <p>
            {report.verified_blobs} of {report.protected_blobs} unique retained
            blob{report.protected_blobs === 1 ? "" : "s"} passed SHA-256 verification.
          </p>
        </Card>
        <Card
          level="default"
          padding="sm"
          eyebrow="ALLOCATION LINEAGE"
          title={`${evidence.allocation_entry_count} entries`}
        >
          <p>
            Operation sequence high-water:
            <strong>{evidence.operation_sequence_high_water}</strong>
          </p>
        </Card>
      </div>

      <section class="authority" aria-label="Verified audit authority">
        <div>
          <span>VAULT ID</span>
          <div class="identity">
            <code>{evidence.vault_id}</code>
            <CopyButton text={evidence.vault_id} ariaLabel="Copy verified vault ID" />
          </div>
        </div>
        <div>
          <span>LINEAGE ID</span>
          <div class="identity">
            <code>{evidence.lineage_id}</code>
            <CopyButton text={evidence.lineage_id} ariaLabel="Copy verified lineage ID" />
          </div>
        </div>
        <div class="wide">
          <span>ALLOCATION HEAD</span>
          <div class="identity">
            <code>{evidence.allocation_head}</code>
            <CopyButton
              text={evidence.allocation_head}
              ariaLabel="Copy verified allocation head"
            />
          </div>
        </div>
      </section>

      {#if metadataProblems.length > 0 || contentProblems.length > 0}
        <section class="problems" aria-label="Audit verification problems">
          <div class="section-heading">
            <span>VERIFICATION PROBLEMS</span>
            <strong>{metadataProblems.length + contentProblems.length} issue{metadataProblems.length + contentProblems.length === 1 ? "" : "s"}</strong>
          </div>
          {#each metadataProblems as problem}
            <p><strong>Metadata</strong> {problem}</p>
          {/each}
          {#each contentProblems as problem}
            <p>
              <strong>{problem.problem}</strong>
              <code>{problem.hash}</code>
            </p>
          {/each}
        </section>
      {/if}

      <section class="scopes" aria-label="Verified audit scope heads">
        <div class="section-heading">
          <span>SCOPE CHAINS</span>
          <strong>{evidence.scopes.length} terminal head{evidence.scopes.length === 1 ? "" : "s"}</strong>
        </div>
        <div class="scope-list">
          {#each visibleScopes as scope (scope.id)}
            <Card
              level="default"
              padding="sm"
              eyebrow={`${scope.entry_count} chain entr${scope.entry_count === 1 ? "y" : "ies"}`}
              title={scope.id}
            >
              <div class="identity">
                <code>{scope.chain_head}</code>
                <CopyButton
                  text={scope.chain_head}
                  ariaLabel={`Copy chain head for scope ${scope.id}`}
                />
              </div>
            </Card>
          {/each}
        </div>
        {#if evidence.scopes.length > visibleScopes.length}
          <p class="limit-note">
            Showing the first {visibleScopes.length} of {evidence.scopes.length}
            verified scopes. Use <code>docbank audit verify --json</code> for
            the complete evidence bundle.
          </p>
        {/if}
      </section>

      <aside class="evidence-note">
        <strong>This is a fresh proof, not an external trust anchor.</strong>
        <p>
          Save <code>docbank audit verify --json</code> outside this vault and
          later pass it back with <code>--expected</code> when you need to prove
          that current history extends an earlier verified state.
        </p>
      </aside>
    {/if}
  </div>
</DetailDrawer>

<style>
  .drawer-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }

  .drawer-heading > div:first-child {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .drawer-heading span,
  .section-heading span,
  .authority > div > span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-bold);
    letter-spacing: var(--letter-spacing-label, 0.04em);
  }

  .drawer-heading strong {
    color: var(--text-primary);
    font-size: var(--font-size-lg);
  }

  .drawer-heading small {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .drawer-actions {
    display: flex;
    gap: var(--space-2);
    flex-shrink: 0;
  }

  .verification {
    display: grid;
    gap: var(--space-5);
    padding: var(--space-5);
  }

  .loading {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .load-error {
    display: grid;
    justify-items: start;
    gap: var(--space-4);
  }

  .load-error p,
  .error {
    margin: 0;
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }

  .result-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: var(--space-4);
    border: 1px solid color-mix(in srgb, var(--accent-green) 42%, var(--border-subtle));
    border-radius: var(--radius-lg);
    background: color-mix(in srgb, var(--accent-green) 8%, var(--bg-raised));
  }

  .result-heading > div {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    color: var(--accent-green);
  }

  .result-heading > div.failed {
    color: var(--accent-red);
  }

  .result-heading span,
  .result-heading strong {
    display: block;
  }

  .result-heading span {
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-bold);
    letter-spacing: var(--letter-spacing-label, 0.04em);
  }

  .result-heading strong {
    color: var(--text-primary);
  }

  .summary-grid,
  .authority {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-3);
  }

  .summary-grid p,
  .evidence-note p {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.45;
  }

  .authority {
    padding: var(--space-4);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-lg);
    background: var(--bg-inset);
  }

  .authority > div,
  .scopes,
  .problems {
    display: grid;
    gap: var(--space-2);
    min-width: 0;
  }

  .authority .wide {
    grid-column: 1 / -1;
  }

  .identity {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--space-2);
    min-width: 0;
  }

  .identity code {
    min-width: 0;
    overflow-wrap: anywhere;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .section-heading {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .section-heading strong {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .scope-list {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-3);
  }

  .problems {
    padding: var(--space-4);
    border: 1px solid color-mix(in srgb, var(--accent-red) 45%, var(--border-subtle));
    border-radius: var(--radius-lg);
    background: color-mix(in srgb, var(--accent-red) 7%, var(--bg-raised));
  }

  .problems p {
    display: grid;
    gap: var(--space-1);
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .problems code {
    overflow-wrap: anywhere;
    font-size: var(--font-size-xs);
  }

  .limit-note {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .evidence-note {
    padding: var(--space-4);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-lg);
    background: var(--bg-inset);
  }

  .evidence-note strong {
    display: block;
    margin-bottom: var(--space-1);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  @media (max-width: 760px) {
    .summary-grid,
    .authority,
    .scope-list {
      grid-template-columns: 1fr;
    }

    .authority .wide {
      grid-column: auto;
    }

    .result-heading {
      align-items: flex-start;
    }
  }
</style>
