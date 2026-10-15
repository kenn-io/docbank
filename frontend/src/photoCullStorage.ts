export type CullScope = "unrated" | "picks" | "rejects" | "all";
export interface CullCheckpoint {
  runID: string;
  pass: string[];
  position: number;
  visited: string[];
  skipped: string[];
  unavailable: string[];
  scope: CullScope;
  autoAdvance: boolean;
}
export interface CullRun {
  ids: string[];
  checkpoint: CullCheckpoint;
}
export interface CullStorage {
  load(): Promise<CullRun | undefined>;
  replace(run: CullRun, signal?: AbortSignal): Promise<void>;
  checkpoint(value: CullCheckpoint): Promise<void>;
}

function request<T>(value: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    value.onsuccess = () => resolve(value.result);
    value.onerror = () => reject(value.error);
  });
}
function complete(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onabort = tx.onerror = () => reject(tx.error ?? new Error("Could not save cull progress."));
  });
}

// Membership and the small checkpoint live separately. Replacing both is atomic.
export function photoCullStorage(vaultID: string): CullStorage {
  async function open() {
    if (!vaultID) throw new Error("The vault identity is unavailable. Reload before culling.");
    const opening = indexedDB.open("docbank-photo-cull-v1", 1);
    opening.onupgradeneeded = () => {
      opening.result.createObjectStore("members");
      opening.result.createObjectStore("checkpoints");
    };
    return request(opening);
  }
  return {
    async load() {
      const db = await open();
      try {
        const tx = db.transaction(["members", "checkpoints"], "readonly");
        const done = complete(tx);
        const [ids, checkpoint] = await Promise.all([
          request<string[] | undefined>(tx.objectStore("members").get(vaultID)),
          request<CullCheckpoint | undefined>(tx.objectStore("checkpoints").get(vaultID)),
        ]);
        await done;
        return ids && checkpoint ? { ids, checkpoint } : undefined;
      } finally {
        db.close();
      }
    },
    async replace(run, signal) {
      const db = await open();
      try {
        const tx = db.transaction(["members", "checkpoints"], "readwrite");
        const done = complete(tx);
        const cancel = () => tx.abort();
        signal?.addEventListener("abort", cancel, { once: true });
        try {
          signal?.throwIfAborted();
          tx.objectStore("members").put(run.ids, vaultID);
          tx.objectStore("checkpoints").put(run.checkpoint, vaultID);
          await done;
        } finally {
          signal?.removeEventListener("abort", cancel);
        }
      } finally {
        db.close();
      }
    },
    async checkpoint(value) {
      const db = await open();
      try {
        const tx = db.transaction("checkpoints", "readwrite");
        const done = complete(tx);
        const store = tx.objectStore("checkpoints");
        const current = await request<CullCheckpoint | undefined>(store.get(vaultID));
        if (current?.runID !== value.runID) {
          tx.abort();
          await done.catch(() => {});
          throw new Error("Another cull run replaced this run. Close and resume it.");
        }
        store.put(value, vaultID);
        await done;
      } finally {
        db.close();
      }
    },
  };
}
