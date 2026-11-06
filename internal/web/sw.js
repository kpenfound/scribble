import { flush } from './outbox.js';
const cacheName = 'scribble-v1';
const shell = ['/', '/index.html', '/style.css', '/app.js', '/outbox.js', '/manifest.webmanifest', '/icon.svg', '/icon-192.png', '/icon-512.png'];
self.addEventListener('install', event => event.waitUntil(caches.open(cacheName).then(cache => cache.addAll(shell)).then(() => self.skipWaiting())));
self.addEventListener('activate', event => event.waitUntil(caches.keys().then(keys => Promise.all(keys.filter(key => key.startsWith('scribble-') && key !== cacheName).map(key => caches.delete(key)))).then(() => self.clients.claim())));
self.addEventListener('fetch', event => {
  const url = new URL(event.request.url);
  if (event.request.method !== 'GET' || url.origin !== self.location.origin || !shell.includes(url.pathname)) return;
  event.respondWith(fetch(event.request).then(response => {
    if (response.ok) { const copy = response.clone(); event.waitUntil(caches.open(cacheName).then(cache => cache.put(event.request, copy))); }
    return response;
  }).catch(() => caches.match(event.request).then(cached => cached || caches.match('/'))));
});
async function send() {
  await flush(async () => {
    for (const client of await self.clients.matchAll({ type: 'window' })) client.postMessage({ type: 'outbox-sent' });
  });
}
self.addEventListener('sync', event => { if (event.tag === 'scribble-outbox') event.waitUntil(send()); });
