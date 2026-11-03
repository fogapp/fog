// The local store: everything the client knows in plaintext, keyed by a
// namespace and a key. IndexedDB in the browser; memory for tests and for
// hosts that bring their own persistence.

export interface Storage {
  get<T = unknown>(ns: string, key: string): Promise<T | undefined>;
  put(ns: string, key: string, value: unknown): Promise<void>;
  // putMany writes several rows in one transaction. Optional: a host's own
  // storage without it gets one put per row.
  putMany?(rows: Array<{ ns: string; key: string; value: unknown }>): Promise<void>;
  del(ns: string, key: string): Promise<void>;
  list<T = unknown>(ns: string): Promise<Array<{ key: string; value: T }>>;
  clear(): Promise<void>;
  // lockName, for storage that several clients (browser tabs) could open at
  // once, names the lock that lets only one of them use it (DeviceLock).
  readonly lockName?: string;
}

export class MemoryStorage implements Storage {
  private m = new Map<string, Map<string, unknown>>();
  private ns(ns: string) {
    let n = this.m.get(ns);
    if (!n) this.m.set(ns, (n = new Map()));
    return n;
  }
  async get<T>(ns: string, key: string) {
    return this.ns(ns).get(key) as T | undefined;
  }
  async put(ns: string, key: string, value: unknown) {
    this.ns(ns).set(key, value);
  }
  async putMany(rows: Array<{ ns: string; key: string; value: unknown }>) {
    for (const r of rows) this.ns(r.ns).set(r.key, r.value);
  }
  async del(ns: string, key: string) {
    this.ns(ns).delete(key);
  }
  async list<T>(ns: string) {
    return Array.from(this.ns(ns).entries()).map(([key, value]) => ({ key, value: value as T }));
  }
  async clear() {
    this.m.clear();
  }
}

// IndexedDBStorage keeps one object store with composite keys [ns, key].
export class IndexedDBStorage implements Storage {
  private db: IDBDatabase;
  readonly lockName: string;
  private constructor(db: IDBDatabase, name: string) {
    this.db = db;
    this.lockName = "fog-chat-device:" + name;
  }

  static open(name = "fog-chat"): Promise<IndexedDBStorage> {
    return new Promise((resolve, reject) => {
      const req = indexedDB.open(name, 1);
      req.onupgradeneeded = () => {
        const db = req.result;
        if (!db.objectStoreNames.contains("kv")) db.createObjectStore("kv", { keyPath: ["ns", "key"] });
      };
      req.onsuccess = () => resolve(new IndexedDBStorage(req.result, name));
      req.onerror = () => reject(req.error);
    });
  }

  private tx(mode: IDBTransactionMode) {
    return this.db.transaction("kv", mode).objectStore("kv");
  }

  private req<T>(r: IDBRequest<T>): Promise<T> {
    return new Promise((resolve, reject) => {
      r.onsuccess = () => resolve(r.result);
      r.onerror = () => reject(r.error);
    });
  }

  async get<T>(ns: string, key: string) {
    const row = await this.req(this.tx("readonly").get([ns, key]));
    return row ? ((row as { value: T }).value as T) : undefined;
  }
  async put(ns: string, key: string, value: unknown) {
    await this.req(this.tx("readwrite").put({ ns, key, value }));
  }
  async putMany(rows: Array<{ ns: string; key: string; value: unknown }>) {
    if (rows.length === 0) return;
    const tx = this.db.transaction("kv", "readwrite");
    const store = tx.objectStore("kv");
    for (const r of rows) store.put({ ns: r.ns, key: r.key, value: r.value });
    await new Promise<void>((resolve, reject) => {
      tx.oncomplete = () => resolve();
      tx.onerror = () => reject(tx.error);
      tx.onabort = () => reject(tx.error);
    });
  }
  async del(ns: string, key: string) {
    await this.req(this.tx("readwrite").delete([ns, key]));
  }
  async list<T>(ns: string) {
    const range = IDBKeyRange.bound([ns, ""], [ns, "￿"]);
    const rows = await this.req(this.tx("readonly").getAll(range));
    return (rows as Array<{ key: string; value: T }>).map((r) => ({ key: r.key, value: r.value }));
  }
  async clear() {
    await this.req(this.tx("readwrite").clear());
  }
}
