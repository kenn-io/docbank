import type { Photos } from "../photos.svelte.js";
export function observeSelectionSize(photos: Photos, onsize: (size: number) => void) {
  return $effect.root(() => {
    const size = $derived(photos.selectedIDs.size);
    $effect(() => onsize(size));
  });
}
