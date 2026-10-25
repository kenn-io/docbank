<script lang="ts">
  import { IconButton } from "@kenn-io/kit-ui";
  import XIcon from "@lucide/svelte/icons/x";
  import { localPreferenceStorage } from "./browser-storage.js";
  import * as generated from "./generated/docbank.js";
  import type { BrowserTelemetryQuietReason } from "@kenn-io/kit-ui/utils/browser-telemetry";

  let {
    session,
    reporting,
    onmanage,
  }: {
    session: string;
    // The reporter that knows whether the daemon accepts usage data.
    reporting?: {
      onQuietChange(
        listener: (quiet: boolean, reason: BrowserTelemetryQuietReason) => void,
      ): () => void;
    };
    onmanage?: () => void;
  } = $props();
  let key = $state("");
  let enabled = $state(false);

  $effect(() => {
    enabled = false;
    return reporting?.onQuietChange((_quiet, reason) => (enabled = reason === "enabled"));
  });

  function dismissed(storageKey: string): boolean {
    try {
      return localPreferenceStorage()?.getItem(storageKey) === "dismissed";
    } catch {
      return false;
    }
  }
  function manage(): void {
    dismiss();
    onmanage?.();
  }
  function dismiss(): void {
    try {
      localPreferenceStorage()?.setItem(key, "dismissed");
    } catch {
      /* Browser storage is optional. */
    }
    key = "";
  }

  $effect(() => {
    const token = session;
    const controller = new AbortController();
    key = "";
    generated
      .auditStatus({}, { session: token, signal: controller.signal })
      .then((status) => {
        const storageKey = `docbank:telemetry-notice:v1:${status.vault_id}`;
        if (!controller.signal.aborted && status.vault_id && !dismissed(storageKey))
          key = storageKey;
      })
      .catch(() => {});
    return () => controller.abort();
  });
</script>

{#if key && enabled}
  <aside class="notice kit-popover-card" aria-label="Anonymous usage">
    <span>Docbank sends anonymous usage data.</span>
    {#if onmanage}<button type="button" class="link" onclick={manage}
        >System → Usage reporting</button
      >{/if}
    <IconButton size="sm" ariaLabel="Dismiss" onclick={dismiss}>
      <XIcon size="14" aria-hidden="true" />
    </IconButton>
  </aside>
{/if}

<style>
  .notice {
    position: fixed;
    right: var(--space-6);
    bottom: var(--space-6);
    z-index: 20;
    display: flex;
    align-items: center;
    gap: var(--space-4);
    max-width: calc(100vw - 2 * var(--space-6));
    padding: var(--space-3) var(--space-3) var(--space-3) var(--space-5);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }
  .link {
    padding: 0;
    border: 0;
    background: none;
    color: var(--accent-blue);
    cursor: pointer;
    font: inherit;
    white-space: nowrap;
  }
  .link:hover {
    text-decoration: underline;
  }
  .link:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 2px;
  }
</style>
