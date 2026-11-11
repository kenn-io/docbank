import { APIError } from "./api-transport.js";

export function retryableHistoryError(cause: unknown): boolean {
  return cause instanceof APIError
    ? cause.status === 408 || cause.status === 429 || (cause.status >= 500 && cause.status < 600)
    : cause instanceof TypeError;
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
    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => {
        signal.removeEventListener("abort", abort);
        resolve();
      }, 500);
      function abort() {
        clearTimeout(timer);
        reject(signal.reason);
      }
      signal.addEventListener("abort", abort, { once: true });
      if (signal.aborted) abort();
    });
    return read();
  }
}
