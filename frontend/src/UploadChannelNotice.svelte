<script lang="ts">
  import { Button, type ButtonSize } from "@kenn-io/kit-ui";
  import type { UploadConnectionState } from "./uploadReconnect.js";

  let {
    state,
    message,
    size = "sm",
    onreconnect,
  }: {
    state: UploadConnectionState;
    message: string;
    size?: ButtonSize;
    onreconnect: () => void;
  } = $props();

  const retrying = $derived(state === "reconnecting" || state === "manual");
</script>

<div
  class="notice"
  class:error={state !== "reconnecting" && state !== "manual"}
  role={retrying ? "status" : "alert"}
>
  {#if retrying || state === "failed"}
    {#if state === "failed"}
      <span class="message"
        >Docbank couldn't reconnect the upload channel. Run `docbank web` again if reconnecting
        fails.</span
      >
    {/if}
    <!-- Keep the same button across retries to preserve keyboard focus. -->
    <Button
      {size}
      class="reconnect"
      tone="info"
      surface="solid"
      busy={retrying}
      label="Reconnect"
      busyLabel="Reconnecting"
      onclick={onreconnect}
    />
  {:else}
    <span class="message">{message}</span>
  {/if}
</div>

<style>
  .notice {
    display: flex;
    flex: 1;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-4);
    min-width: 0;
  }
  .message {
    flex: 1;
  }
  .notice :global(.reconnect) {
    margin-left: auto;
  }
  .error .message {
    color: var(--accent-red);
  }
</style>
