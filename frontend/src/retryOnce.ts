export async function retryOnce<T>(
  run: () => Promise<T>,
  shouldRetry: (cause: unknown) => boolean,
  wait?: () => Promise<void>,
): Promise<T> {
  try {
    return await run();
  } catch (cause) {
    if (!shouldRetry(cause)) throw cause;
    if (wait) await wait();
    return run();
  }
}
