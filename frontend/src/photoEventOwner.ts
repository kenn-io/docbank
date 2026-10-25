const photoEventOwner = "docbank-photo-events";

// The owner forwards each change to the other tabs.
let channel: BroadcastChannel | undefined;
const photoEventChannel = () => (channel ??= new BroadcastChannel(photoEventOwner));

export function announcePhotoEvent() {
  photoEventChannel().postMessage(null);
}

export function onPhotoEventSignal(receive: () => void) {
  const listener = () => receive();
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
