<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import type { PersistedAction } from "./actionJournal.js";
  import type { Tag } from "./generated/docbank.js";
  import { documentCount } from "./tagChangeText.js";

  interface Props {
    action: PersistedAction | "unreadable";
    tags: readonly Tag[];
    busy: boolean;
    error?: string;
    onresume: () => void;
    ondiscard: () => void;
  }

  let { action, tags, busy, error = "", onresume, ondiscard }: Props = $props();
  let confirming = $state(false);

  const message = $derived.by(() => {
    if (action === "unreadable") return "An unfinished tagging run can't be read.";
    const name = tags.find((tag) => tag.id === action.tag_id)?.name ?? "a tag";
    const label = name === "a tag" ? name : `“${name}”`;
    return action.assign
      ? `Adding ${label} to ${documentCount(action.total)} didn't finish.`
      : `Removing ${label} from ${documentCount(action.total)} didn't finish.`;
  });
</script>

<div class="banner tag-run-notice" role="status">
  {#if confirming}
    <span>Discard this run? Changes already made stay.</span>
    <Button
      size="sm"
      tone="danger"
      disabled={busy}
      onclick={() => {
        confirming = false;
        ondiscard();
      }}>Discard</Button
    >
    <Button size="sm" surface="soft" disabled={busy} onclick={() => (confirming = false)}
      >Keep</Button
    >
  {:else}
    <span>{message}</span>
    {#if action !== "unreadable"}
      <Button size="sm" tone="info" disabled={busy} onclick={onresume}>Resume</Button>
    {/if}
    <Button size="sm" surface="soft" disabled={busy} onclick={() => (confirming = true)}
      >Discard…</Button
    >
  {/if}
  {#if error}<span class="error" role="alert">{error}</span>{/if}
</div>

<style>
  .tag-run-notice {
    gap: var(--space-3);
    flex-wrap: wrap;
    padding-block: var(--space-3);
    border-bottom: 1px solid var(--border-muted);
    background: color-mix(in srgb, var(--accent-amber, var(--accent-blue)) 8%, var(--bg-surface));
    color: var(--text-primary);
  }
  .tag-run-notice > span:first-child {
    margin-inline-end: var(--space-2);
  }
</style>
