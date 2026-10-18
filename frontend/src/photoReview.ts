import { tick } from "svelte";

export async function restorePhotoReviewFocus(element: () => HTMLElement | undefined) {
  await tick();
  element()?.focus({ preventScroll: true });
}
