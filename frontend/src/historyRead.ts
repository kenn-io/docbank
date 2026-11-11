import { APIError, NetworkError } from "./api-transport.js";
import { pause } from "./pageViewer.js";

export function retryableHistoryError(cause: unknown): boolean {
  return cause instanceof APIError
    ? cause.status === 408 || cause.status === 429 || (cause.status >= 500 && cause.status < 600)
    : cause instanceof NetworkError;
}

export async function readHistory<T>(
  read: () => Promise<T>,
  signal: AbortSignal,
  retry = true,
): Promise<T> {
  try {
    return await read();
  } catch (cause) {
    if (!retry || signal.aborted || !retryableHistoryError(cause)) throw cause;
    await pause(signal);
    return read();
  }
}
