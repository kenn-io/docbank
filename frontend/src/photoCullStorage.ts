import { idbRequest, transactionDone } from "./indexedDB.js";

export type CullScope = "unflagged" | "picks" | "rejects" | "all";
export interface CullCheckpoint {
  runID: string;
  passID: string;
  revision: number;
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
type MarkerKind = "visited" | "skipped" | "unavailable";
type Header = Pick<
  CullCheckpoint,
  "runID" | "passID" | "revision" | "position" | "scope" | "autoAdvance"
>;
export interface CullProgress {
  runID: string;
  passID: string;
  revision: number;
  position?: number;
  autoAdvance?: boolean;
  marker?: { kind: MarkerKind; id: string; index: number };
  pass?: CullCheckpoint;
}
export interface CullStorage {
  load(): Promise<CullRun | undefined>;
  replace(run: CullRun, signal?: AbortSignal, expected?: CullCheckpoint): Promise<void>;
  checkpoint(value: CullProgress): Promise<void>;
}

const sameCheckpoint = (
  a: Pick<CullCheckpoint, "runID" | "passID" | "revision">,
  b: Pick<CullCheckpoint, "runID" | "passID" | "revision">,
) => a.runID === b.runID && a.passID === b.passID && a.revision === b.revision;

const stores = ["members", "checkpoints", "markers"];
const markerKinds: MarkerKind[] = ["visited", "skipped", "unavailable"];
const header = ({
  runID,
  passID,
  revision,
  position,
  scope,
  autoAdvance,
}: CullCheckpoint): Header => ({
  runID,
  passID,
  revision,
  position,
  scope,
  autoAdvance,
});

// Full membership/pass writes happen only at capture and pass boundaries.
export function photoCullStorage(vaultID: string): CullStorage {
  const markerRange = () => IDBKeyRange.bound([vaultID], [vaultID, []]);
  async function open() {
    if (!vaultID) throw new Error("The vault identity is unavailable. Reload before culling.");
    const opening = indexedDB.open("docbank-photo-cull-v1", 1);
    opening.onupgradeneeded = () => {
      for (const name of stores) opening.result.createObjectStore(name);
    };
    return idbRequest(opening);
  }
  async function savePass(tx: IDBTransaction, checkpoint: CullCheckpoint) {
    const markers = tx.objectStore("markers");
    const keys = await idbRequest(markers.getAllKeys(markerRange()));
    for (const key of keys) markers.delete(key);
    for (const kind of markerKinds)
      checkpoint[kind].forEach((id, index) => markers.put(id, [vaultID, kind, index]));
    tx.objectStore("checkpoints").put(header(checkpoint), vaultID);
  }
  return {
    async load() {
      const db = await open();
      try {
        const tx = db.transaction(stores, "readonly"),
          done = transactionDone(tx);
        const [membership, saved, keys, values] = await Promise.all([
          idbRequest<{ ids: string[]; pass: string[] } | undefined>(
            tx.objectStore("members").get(vaultID),
          ),
          idbRequest<Header | undefined>(tx.objectStore("checkpoints").get(vaultID)),
          idbRequest(tx.objectStore("markers").getAllKeys(markerRange())),
          idbRequest<string[]>(tx.objectStore("markers").getAll(markerRange())),
        ]);
        await done;
        if (!membership || !saved) return;
        const checkpoint: CullCheckpoint = {
          ...saved,
          pass: membership.pass,
          visited: [],
          skipped: [],
          unavailable: [],
        };
        keys.forEach((key, index) =>
          checkpoint[(key as [string, MarkerKind, number])[1]].push(values[index]!),
        );
        return { ids: membership.ids, checkpoint };
      } finally {
        db.close();
      }
    },
    async replace(run, signal, expected) {
      const db = await open();
      try {
        const tx = db.transaction(stores, "readwrite"),
          done = transactionDone(tx);
        const cancel = () => tx.abort();
        signal?.addEventListener("abort", cancel, { once: true });
        try {
          signal?.throwIfAborted();
          const current = await idbRequest<Header | undefined>(
            tx.objectStore("checkpoints").get(vaultID),
          );
          if (current ? !expected || !sameCheckpoint(current, expected) : !!expected) {
            tx.abort();
            await done.catch(() => {});
            throw new Error("Cull progress changed in another tab. Resume before replacing it.");
          }
          tx.objectStore("members").put({ ids: run.ids, pass: run.checkpoint.pass }, vaultID);
          await savePass(tx, run.checkpoint);
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
        const tx = db.transaction(value.pass ? stores : ["checkpoints", "markers"], "readwrite"),
          done = transactionDone(tx);
        const store = tx.objectStore("checkpoints");
        const current = await idbRequest<Header | undefined>(store.get(vaultID));
        if (!current || !sameCheckpoint(current, value)) {
          tx.abort();
          await done.catch(() => {});
          throw new Error("Cull progress changed in another tab. Close and resume it.");
        }
        if (value.pass) {
          const members = tx.objectStore("members");
          const membership = await idbRequest<{ ids: string[]; pass: string[] }>(
            members.get(vaultID),
          );
          members.put({ ...membership, pass: value.pass.pass }, vaultID);
          await savePass(tx, { ...value.pass, revision: current.revision + 1 });
        } else {
          if (value.position !== undefined) current.position = value.position;
          if (value.autoAdvance !== undefined) current.autoAdvance = value.autoAdvance;
          current.revision++;
          store.put(current, vaultID);
          if (value.marker) {
            const { kind, id, index } = value.marker;
            tx.objectStore("markers").put(id, [vaultID, kind, index]);
          }
        }
        await done;
      } finally {
        db.close();
      }
    },
  };
}
