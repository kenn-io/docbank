import { tick } from "svelte";
import type { Photos } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";

export interface PhotoWorkspace {
  photos: Photos;
  cache: PhotoPreviewCache;
}

export function concealPhotoWorkspace(workspace?: PhotoWorkspace) {
  workspace?.photos.cancelPending();
  return workspace?.cache.dispose();
}

export async function recreatePhotoPreviewCache(
  workspace: PhotoWorkspace,
  session: string,
  onauthfailure: (cause: unknown) => void,
  valid: () => boolean,
) {
  return replacePhotoPreviewCache(workspace, session, onauthfailure, valid, false);
}

export async function revalidatePhotoWorkspace(
  workspace: PhotoWorkspace,
  session: string,
  onauthfailure: (cause: unknown) => void,
  valid: () => boolean,
) {
  return replacePhotoPreviewCache(workspace, session, onauthfailure, valid, true);
}

async function replacePhotoPreviewCache(
  workspace: PhotoWorkspace,
  session: string,
  onauthfailure: (cause: unknown) => void,
  valid: () => boolean,
  refresh: boolean,
) {
  await workspace.cache.dispose();
  await tick();
  if (!valid()) return false;
  if (refresh) {
    const refreshed =
      workspace.photos.projection === "map" && workspace.photos.staged
        ? await workspace.photos.retry()
        : await workspace.photos.refresh();
    if (!valid() || workspace.photos.hiding || workspace.photos.trashing) return false;
    const error = workspace.photos.error;
    if (error) throw new Error(error);
    if (!refreshed) return false;
  }
  workspace.cache = new PhotoPreviewCache(session, onauthfailure, workspace.photos.hidden);
  return true;
}
