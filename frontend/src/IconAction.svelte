<script lang="ts">
  import type { Snippet } from "svelte";
  import { IconButton } from "@kenn-io/kit-ui";
  import AppTooltip from "./AppTooltip.svelte";

  // An icon-only button with the app tooltip. The browser's own title bubble
  // is blanked so one tooltip shows, at one delay. `tip` replaces the label
  // when a disabled button should say why instead of what it would do.
  let {
    ariaLabel,
    tip,
    size = "md",
    tone,
    disabled = false,
    ariaPressed,
    ariaExpanded,
    ariaControls,
    onclick,
    children,
  }: {
    ariaLabel: string;
    tip?: string;
    size?: "sm" | "md";
    tone?: "neutral" | "success" | "danger" | "info" | "workflow";
    disabled?: boolean;
    ariaPressed?: boolean;
    ariaExpanded?: boolean;
    ariaControls?: string;
    onclick?: (event: MouseEvent) => void;
    children: Snippet;
  } = $props();
</script>

<AppTooltip text={tip ?? ariaLabel}>
  <IconButton
    {ariaLabel}
    title=""
    {size}
    {tone}
    {disabled}
    {ariaPressed}
    {ariaExpanded}
    {ariaControls}
    {onclick}>{@render children()}</IconButton
  >
</AppTooltip>
