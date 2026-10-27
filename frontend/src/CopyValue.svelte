<script lang="ts">
  import { Button, copyToClipboard } from "@kenn-io/kit-ui";
  import CopyIcon from "@lucide/svelte/icons/copy";
  import CheckIcon from "@lucide/svelte/icons/check";

  let { value, label, mono = true }: { value: string; label: string; mono?: boolean } = $props();
  let feedback = $state("");
  let generation = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;

  $effect(() => {
    void value;
    generation++;
    feedback = "";
    return () => {
      generation++;
      clearTimeout(timer);
    };
  });

  async function copy() {
    const request = ++generation;
    const copied = await copyToClipboard(value);
    if (request !== generation) return;
    feedback = copied ? "Copied" : "Couldn't copy";
    clearTimeout(timer);
    timer = setTimeout(() => (feedback = ""), 1500);
  }
</script>

<Button
  size="sm"
  surface="soft"
  class={`copy-value ${mono ? "copy-value--mono" : ""}`}
  ariaLabel={`Copy ${label}`}
  title={value}
  onclick={() => void copy()}
>
  <span class="value">{value}</span>
  <span class:visible={!!feedback} class="copy-state" aria-hidden="true">
    {#if feedback === "Copied"}<CheckIcon size={13} />{:else}<CopyIcon size={13} />{/if}
  </span>
  <span class="feedback" role="status">{feedback}</span>
</Button>

<style>
  :global(.copy-value.kit-button.kit-button--soft.kit-button--neutral) {
    width: 100%;
    min-width: 0;
    min-height: 24px;
    justify-content: flex-start;
    gap: var(--space-2);
    padding: 2px 4px;
    margin: -2px -4px;
    border-color: transparent;
    background: transparent;
    box-shadow: none;
    color: inherit;
    font-size: inherit;
    font-weight: inherit;
    text-align: left;
    cursor: pointer;
  }
  /* The hover look must not add a second border or tint inside a bordered host. */
  :global(.copy-value.kit-button.kit-button--soft.kit-button--neutral:hover:not(:disabled)) {
    border-color: transparent;
    box-shadow: none;
  }
  :global(.copy-value--mono) .value {
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }
  .value {
    min-width: 0;
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
  }
  .copy-state {
    display: inline-flex;
    flex: none;
    margin-left: auto;
    color: var(--text-muted);
    opacity: 0;
  }
  .copy-state.visible,
  :global(.copy-value:hover) .copy-state,
  :global(.copy-value:focus-visible) .copy-state {
    opacity: 1;
  }
  :global(.copy-value:focus-visible) {
    outline: 2px solid var(--accent-blue);
    outline-offset: 2px;
  }
  .feedback {
    font-family: var(--font-sans);
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }
  @media (hover: none) {
    .copy-state {
      opacity: 1;
    }
  }
</style>
