const photoEventOwner = "docbank-photo-events";

// The owner forwards each event to the other tabs. The Hidden unlock cookie is shared
// across them, so every tab may see the same photos and the same IDs.
let channel: BroadcastChannel | undefined;
const photoEventChannel = () => (channel ??= new BroadcastChannel(photoEventOwner));

export function announcePhotoEvent(event: object) {
  photoEventChannel().postMessage(event);
}

export function onPhotoEventSignal(receive: (event: unknown) => void) {
  const listener = ({ data }: MessageEvent) => receive(data);
  photoEventChannel().addEventListener("message", listener);
  return () => photoEventChannel().removeEventListener("message", listener);
}

// Browsers without Web Locks open one stream per visible tab.
export async function withPhotoEventOwnership(
  signal: AbortSignal,
  consume: (signal: AbortSignal) => Promise<void>,
) {
  if (typeof navigator.locks === "undefined") await consume(signal);
  else await navigator.locks.request(photoEventOwner, { signal }, () => consume(signal));
}
