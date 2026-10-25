<script lang="ts">
  import { Button, IconButton } from "@kenn-io/kit-ui";
  import XIcon from "@lucide/svelte/icons/x";
  import { dismissToast, getToasts, holdToast, releaseToast } from "./toast.svelte.js";

  const toasts = $derived(getToasts());

  async function act(id: number, run: () => void | Promise<void>): Promise<void> {
    dismissToast(id);
    await run();
  }
</script>

{#if toasts.length > 0}
  <div class="stack" role="region" aria-label="Notifications">
    {#each toasts as toast (toast.id)}
      <div
        class="card kit-popover-card"
        class:danger={toast.tone === "danger"}
        role={toast.tone === "danger" ? "alert" : "status"}
        onmouseenter={() => holdToast(toast.id)}
        onmouseleave={() => releaseToast(toast.id)}
        onfocusin={() => holdToast(toast.id)}
        onfocusout={() => releaseToast(toast.id)}
      >
        <span class="message">{toast.message}</span>
        {#if toast.action}
          {@const action = toast.action}
          <Button size="sm" surface="soft" onclick={() => void act(toast.id, action.run)}
            >{action.label}</Button
          >
        {/if}
        <IconButton size="sm" ariaLabel="Dismiss" onclick={() => dismissToast(toast.id)}>
          <XIcon size="14" aria-hidden="true" />
        </IconButton>
      </div>
    {/each}
  </div>
{/if}

<style>
  .stack {
    position: fixed;
    bottom: var(--space-7);
    left: 50%;
    z-index: var(--z-tooltip);
    display: grid;
    gap: var(--space-4);
    justify-items: center;
    max-width: min(560px, calc(100vw - 32px));
    transform: translateX(-50%);
  }
  .card {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    padding: var(--space-4) var(--space-4) var(--space-4) var(--space-6);
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }
  .card.danger {
    border-color: color-mix(in srgb, var(--accent-red) 40%, var(--border-default));
    color: var(--accent-red);
  }
  .message {
    min-width: 0;
    overflow-wrap: anywhere;
  }
</style>
