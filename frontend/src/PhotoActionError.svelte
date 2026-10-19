<script lang="ts">
  import { Notice } from "@kenn-io/kit-ui";

  let { message, ondismiss }: { message: string; ondismiss: () => void } = $props();
</script>

{#if message}
  <div class="photo-action-error">
    <Notice tone="error" {message} actionLabel="Dismiss" onaction={ondismiss} />
  </div>
{/if}

<style>
  .photo-action-error {
    margin: var(--space-3) var(--space-5);
  }
</style>
