const database = new Promise((resolve, reject) => {
  const open = indexedDB.open('scribble', 1);
  open.onupgradeneeded = () => {
    const store = open.result.createObjectStore('outbox', { keyPath: 'sequence', autoIncrement: true });
    store.createIndex('id', 'id', { unique: true });
    open.result.createObjectStore('meta');
  };
  open.onsuccess = () => resolve(open.result);
  open.onerror = () => reject(open.error);
});
async function transaction(store, mode, perform) {
  const db = await database;
  return new Promise((resolve, reject) => {
    const tx = db.transaction(store, mode);
    const req = perform(tx.objectStore(store));
    tx.oncomplete = () => resolve(req?.result);
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error || new Error('Local storage transaction aborted'));
  });
}
export const queued = () => transaction('outbox', 'readonly', s => s.getAll());
export const enqueue = item => transaction('outbox', 'readwrite', s => s.add(item));
export const forget = sequence => transaction('outbox', 'readwrite', s => s.delete(sequence));
export const getMeta = key => transaction('meta', 'readonly', s => s.get(key));
export const setMeta = (key, value) => transaction('meta', 'readwrite', s => s.put(value, key));
export async function flush(notify = () => {}) {
  const run = async () => {
    for (const item of await queued()) {
      let body, headers;
      if (item.audio) {
        body = new FormData();
        body.set('id', item.id); body.set('captured_at', item.captured_at);
        body.set('audio', item.audio, 'capture');
      } else {
        body = JSON.stringify({ id: item.id, captured_at: item.captured_at, text: item.text });
        headers = { 'Content-Type': 'application/json' };
      }
      let response;
      try { response = await fetch('/v1/thoughts', { method: 'POST', headers, body }); }
      catch { throw new Error('Saved on this device. It will send when Scribble is reachable.'); }
      if (response.status !== 202) {
        let message = `Server returned ${response.status}`;
        // The server answers failures with {"error":{"code","message"}}.
        try { const serverMessage = (await response.json()).error?.message; if (typeof serverMessage === 'string' && serverMessage) message = serverMessage; } catch { /* preserve status */ }
        throw new Error(`${message}. Your capture is still saved on this device.`);
      }
      const receipt = await response.json();
      if (receipt.id !== item.id) throw new Error('Unexpected acknowledgement; keeping your capture on this device.');
      await forget(item.sequence); // Only a durable server acknowledgement removes it.
      await notify(receipt);
    }
  };
  if (navigator.locks) return navigator.locks.request('scribble-send', run);
  return run(); // UUID idempotency also protects browsers without Web Locks.
}
