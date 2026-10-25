<script lang="ts">
  import { getContext, type Snippet } from "svelte";
  import { DetailDrawer } from "@kenn-io/kit-ui";
  import DrawerHeading from "./DrawerHeading.svelte";
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
        <DrawerHeading
          title={navigation ? "System" : title}
          {actions}
          {onclose}
          closeLabel={navigation ? "Close System" : closeLabel}
        />
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
</style>
