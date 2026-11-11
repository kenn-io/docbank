<script lang="ts">
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import type { UploadConnectionState } from "./uploadReconnect.js";

  let {
    state,
    message,
    onreconnect,
  }: {
    state: UploadConnectionState;
    message: string;
    onreconnect: () => void;
  } = $props();
</script>

<div class="notice" role={state === "reconnecting" || state === "manual" ? "status" : "alert"}>
  {#if state === "reconnecting"}
    <Spinner size={12} /> The upload channel disconnected. Reconnecting…
  {:else if state === "failed" || state === "manual"}
    Docbank couldn't reconnect the upload channel.
    <Button size="sm" disabled={state === "manual"} onclick={onreconnect}>
      {#if state === "manual"}<Spinner size={12} /> Reconnecting…{:else}Reconnect{/if}
    </Button>
  {:else}
    {message}
  {/if}
</div>

<style>
  .notice {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-4);
  }
</style>
