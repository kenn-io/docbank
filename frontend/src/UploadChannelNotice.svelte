<script lang="ts">
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import { slide } from "svelte/transition";
  import type { UploadConnectionState } from "./uploadReconnect.js";

  let {
    visible = true,
    state,
    message,
    onreconnect,
  }: {
    visible?: boolean;
    state: UploadConnectionState;
    message: string;
    onreconnect: () => void;
  } = $props();

  // Collapse instead of snapping so controls below don't jump under the pointer.
  const duration = window.matchMedia("(prefers-reduced-motion: reduce)").matches ? 0 : 150;
</script>

{#if visible}
  <div
    class="notice"
    class:error={state !== "reconnecting"}
    role={state === "reconnecting" || state === "manual" ? "status" : "alert"}
    transition:slide={{ duration }}
  >
    {#if state === "reconnecting"}
      <span class="message"
        ><Spinner size={12} /> The upload channel disconnected. Reconnecting…</span
      >
      <!-- Holds the Reconnect button's height so the row doesn't grow if retries run out. -->
      <span class="reserve" aria-hidden="true"><Button size="sm" disabled>Reconnect</Button></span>
    {:else if state === "failed" || state === "manual"}
      <span class="message">Docbank couldn't reconnect the upload channel.</span>
      <Button
        size="sm"
        tone="info"
        surface="solid"
        disabled={state === "manual"}
        onclick={onreconnect}
      >
        {#if state === "manual"}<Spinner size={12} /> Reconnecting…{:else}Reconnect{/if}
      </Button>
    {:else}
      <span class="message">{message}</span>
    {/if}
  </div>
{/if}

<style>
  .notice {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-4);
  }
  .message {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }
  .error .message {
    color: var(--accent-red);
  }
  .reserve {
    visibility: hidden;
  }
</style>
