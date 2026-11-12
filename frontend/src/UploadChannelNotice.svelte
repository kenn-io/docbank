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
    <span class="message"
      >{retrying
        ? "The upload channel disconnected. Reconnecting…"
        : "Docbank couldn't reconnect the upload channel."}</span
    >
    <!-- One button across retries keeps its width, place, and keyboard focus. -->
    <Button
      {size}
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
  .error .message {
    color: var(--accent-red);
  }
</style>
