<script lang="ts">
  import { onMount } from "svelte";
  import FileTextIcon from "@lucide/svelte/icons/file-text";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, Card, Chip, DetailDrawer, IconButton, Spinner } from "@kenn-io/kit-ui";
  import { APIError, renditionArtifact, type RenditionArtifact } from "./api.js";

  interface Props {
    session: string;
    attachmentID: string;
    path: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, attachmentID, path, onclose, onauthfailure }: Props = $props();
  let rendition = $state<RenditionArtifact | null>(null);
  let loading = $state(true);
  let error = $state("");
  let generation = 0;

  onMount(() => {
    void load();
    return () => { generation += 1; };
  });

  async function load(): Promise<void> {
    const request = ++generation;
    loading = true;
    error = "";
    try {
      const next = await renditionArtifact(session, attachmentID);
      if (request !== generation) return;
      rendition = next;
    } catch (cause) {
      if (request !== generation) return;
      if (cause instanceof APIError && cause.status === 401) {
        onauthfailure(cause);
        onclose();
        return;
      }
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  function titleCase(value: string): string {
    return value ? value.replaceAll("_", " ").replace(/^./, (char) => char.toUpperCase()) : "Unknown";
  }
</script>

<DetailDrawer width="min(880px, 100vw)" ariaLabel="Sanitized Markdown rendition" {onclose}>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>SANITIZED MARKDOWN</span>
        <strong>{path}</strong>
        <small>Verified retained bytes · active attachment {attachmentID.slice(0, 12)}…</small>
      </div>
      <div class="drawer-actions">
        <IconButton size="sm" ariaLabel="Refresh sanitized Markdown" disabled={loading} onclick={() => void load()}>
          <RefreshCwIcon size="14" aria-hidden="true" />
        </IconButton>
        <IconButton size="sm" ariaLabel="Close sanitized Markdown" onclick={onclose}>
          <XIcon size="14" aria-hidden="true" />
        </IconButton>
      </div>
    </div>
  {/snippet}

  <div class="rendition-shell">
    {#if loading && !rendition}
      <div class="loading"><Spinner size={16} /> Verifying retained Markdown…</div>
    {:else if error && !rendition}
      <div class="load-error">
        <p role="alert">{error}</p>
        <p>The active rendition has no readable verified mirror right now.</p>
        <Button size="sm" onclick={() => void load()}>Try again</Button>
      </div>
    {:else if rendition}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <div class="identity-grid">
        <Card level="default" padding="sm" eyebrow="COMPLETENESS" title={titleCase(rendition.completeness)}>
          {#snippet actions()}<Chip size="xs" tone={rendition?.completeness === "complete" ? "success" : "warning"}>{titleCase(rendition?.completeness ?? "")}</Chip>{/snippet}
          <p>The YAML identity envelope was verified and is hidden from document prose.</p>
        </Card>
        <Card level="default" padding="sm" eyebrow="BUILD IDENTITY" title="Immutable rendition build">
          {#snippet actions()}<FileTextIcon size="18" aria-hidden="true" />{/snippet}
          <code>{rendition.buildID}</code>
          <small>Source version {rendition.contentVersionID}</small>
        </Card>
        <Card level="default" padding="sm" eyebrow="SOURCE & PROFILE" title={`${rendition.source.format} · ${rendition.source.mediaType}`}>
          <code>{rendition.profileFingerprint}</code>
          <small>Source SHA-256 {rendition.source.sha256}</small>
        </Card>
      </div>
      {#if rendition.warnings.length > 0}
        <div class="warnings" role="status">
          <strong>Rendition warnings</strong>
          {#each rendition.warnings as warning}<span>{warning}</span>{/each}
        </div>
      {/if}
      {#if rendition.navigation.entries.length > 0}
        <section class="navigation" aria-label="Rendition navigation">
          <div>
            <strong>Body navigation</strong>
            <Chip size="xs" tone={rendition.navigation.complete ? "success" : "warning"}>{rendition.navigation.complete ? "Complete" : "Bounded"}</Chip>
          </div>
          {#each rendition.navigation.entries as entry}
            <div class="navigation-entry">
              <span>{entry.title || entry.key}</span>
              <small>{entry.kind} · line {entry.line} · body-relative byte {entry.byte}</small>
            </div>
          {/each}
        </section>
      {/if}
      <article class="rendition-document" aria-label="Sanitized Markdown document">
        <pre class="rendition-body">{rendition.markdown}</pre>
      </article>
    {/if}
  </div>
</DetailDrawer>

<style>
  .drawer-heading { display: flex; align-items: center; justify-content: space-between; gap: var(--space-4); width: 100%; }
  .drawer-heading > div:first-child { display: grid; gap: var(--space-1); min-width: 0; }
  .drawer-heading span { color: var(--text-muted); font-size: var(--font-size-xs); font-weight: var(--font-weight-bold); letter-spacing: .04em; }
  .drawer-heading strong { overflow: hidden; color: var(--text-primary); font-size: var(--font-size-lg); text-overflow: ellipsis; white-space: nowrap; }
  .drawer-heading small, .identity-grid small { color: var(--text-muted); font-size: var(--font-size-xs); }
  .drawer-actions { display: flex; gap: var(--space-2); }
  .rendition-shell { display: grid; gap: var(--space-4); padding: var(--space-5); }
  .loading { display: flex; align-items: center; gap: var(--space-3); color: var(--text-secondary); }
  .load-error { display: grid; justify-items: start; gap: var(--space-3); }
  .load-error p, .error { margin: 0; color: var(--accent-red); }
  .identity-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(230px, 1fr)); gap: var(--space-3); }
  .identity-grid p, .identity-grid code { margin: 0; overflow-wrap: anywhere; color: var(--text-secondary); font-size: var(--font-size-sm); }
  .identity-grid :global(.kit-card-content) { display: grid; gap: var(--space-2); }
  .warnings { display: grid; gap: var(--space-1); padding: var(--space-3); border: 1px solid var(--accent-amber); border-radius: var(--radius-md); color: var(--text-secondary); }
  .navigation { display: grid; gap: var(--space-2); padding: var(--space-3); border: 1px solid var(--border-subtle); border-radius: var(--radius-md); }
  .navigation > div:first-child, .navigation-entry { display: flex; justify-content: space-between; gap: var(--space-3); }
  .navigation-entry { padding-top: var(--space-2); border-top: 1px solid var(--border-subtle); color: var(--text-secondary); }
  .rendition-document { min-height: 320px; padding: clamp(1rem, 4vw, 3rem); border: 1px solid var(--border-subtle); border-radius: var(--radius-lg); background: var(--surface-raised); }
  .rendition-body { margin: 0; overflow-wrap: anywhere; white-space: pre-wrap; color: var(--text-primary); font-family: var(--font-family-sans); font-size: var(--font-size-md); line-height: 1.65; }
  @media (max-width: 640px) { .identity-grid { grid-template-columns: 1fr; } }
</style>
