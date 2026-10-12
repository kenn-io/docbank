import { tick } from "svelte";
import type { Photos } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";

export interface PhotoWorkspace { photos: Photos; cache: PhotoPreviewCache }

export function concealPhotoWorkspace(workspace?: PhotoWorkspace) {
  workspace?.photos.cancelPending();
  return workspace?.cache.dispose();
}

export async function revalidatePhotoWorkspace(workspace: PhotoWorkspace, session: string, onauthfailure: (cause: unknown) => void, valid: () => boolean) {
  await workspace.cache.dispose();
  await tick();
  if (!valid()) return false;
  const refreshed = await workspace.photos.refresh();
  if (!valid() || workspace.photos.hiding || workspace.photos.trashing) return false;
  if (workspace.photos.error) throw new Error(workspace.photos.error);
  if (!refreshed) return false;
  workspace.cache = new PhotoPreviewCache(session, onauthfailure, workspace.photos.hidden);
  return true;
}
