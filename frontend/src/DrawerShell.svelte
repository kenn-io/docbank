<script lang="ts">
  import { getContext, type Snippet } from "svelte";
  import { DetailDrawer } from "@kenn-io/kit-ui";
  import XIcon from "@lucide/svelte/icons/x";
  import IconAction from "./IconAction.svelte";
  import { drawerNavigation, type DrawerNavigation } from "./drawerNavigation.js";

  let {
    title,
    ariaLabel = title,
    onclose,
    closeLabel = "Close",
    closeOnOverlayClick = true,
    actions,
    children,
    footer,
  }: {
    title: string;
    ariaLabel?: string;
    onclose: () => void;
    closeLabel?: string;
    closeOnOverlayClick?: boolean;
    actions?: Snippet;
    children?: Snippet;
    footer?: Snippet;
  } = $props();
  const navigation = getContext<DrawerNavigation | undefined>(drawerNavigation);
  const still = navigation?.switched ?? false;
</script>

<div class="shell" class:still>
  <DetailDrawer
    width="min(720px, 100vw)"
    ariaLabel={navigation ? "System" : ariaLabel}
    {onclose}
    {closeOnOverlayClick}
    {footer}
  >
    {#snippet header()}
      <div class="header">
        <div class="heading">
          <h2>{navigation ? "System" : title}</h2>
          <div class="actions">
            {@render actions?.()}
            <IconAction
              size="sm"
              ariaLabel={navigation ? "Close System" : closeLabel}
              tip="Close (Esc)"
              onclick={onclose}
            >
              <XIcon size="16" aria-hidden="true" />
            </IconAction>
          </div>
        </div>
        {@render navigation?.tabs()}
      </div>
    {/snippet}
    {@render children?.()}
  </DetailDrawer>
</div>

<style>
  .shell {
    display: contents;
  }
  .still :global(.kit-detail-drawer) {
    animation: none;
  }
  .header {
    display: grid;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }
  .heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }
  h2 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-shrink: 0;
  }
</style>
