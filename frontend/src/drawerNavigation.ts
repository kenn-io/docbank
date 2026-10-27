import type { Snippet } from "svelte";

export const drawerNavigation = Symbol("drawer-navigation");
export interface DrawerNavigation {
  tabs: Snippet;
  /** True once the user has switched tabs, so a remounted shell skips the slide-in. */
  readonly switched: boolean;
}

export const drawerPage = Symbol("drawer-page");
