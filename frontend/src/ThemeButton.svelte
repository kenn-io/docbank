<script lang="ts">
  import { getThemeMode, setThemeMode, type ThemeMode } from "@kenn-io/kit-ui";
  import MonitorIcon from "@lucide/svelte/icons/monitor";
  import MoonIcon from "@lucide/svelte/icons/moon";
  import SunIcon from "@lucide/svelte/icons/sun";
  import IconAction from "./IconAction.svelte";

  // kit-ui's cycling theme button, with the app tooltip in place of the browser's.
  const next: Record<ThemeMode, ThemeMode> = { light: "dark", dark: "system", system: "light" };
  const names: Record<ThemeMode, string> = { light: "Light", dark: "Dark", system: "System" };
  const mode = $derived(getThemeMode());
</script>

<IconAction
  ariaLabel={`Change theme (current: ${names[mode]})`}
  tip={`Theme: ${names[mode]}`}
  onclick={() => setThemeMode(next[mode])}
>
  {#if mode === "light"}<SunIcon size="16" aria-hidden="true" />
  {:else if mode === "dark"}<MoonIcon size="16" aria-hidden="true" />
  {:else}<MonitorIcon size="16" aria-hidden="true" />{/if}
</IconAction>
