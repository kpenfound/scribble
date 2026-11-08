import { enqueue, queued, flush, getMeta, setMeta } from './outbox.js';
const $ = id => document.getElementById(id);
let jobs = [], today = '', selected = null, current = null, sending = false, recording = null, recordTimer = null;
let registration, statusEvents = 0;
function notice(message = '') { $('notice').textContent = message; $('notice').hidden = !message; }
async function render() {
  const local = await queued();
  $('outbox').hidden = local.length === 0;
  $('outbox').textContent = `${local.length} ${local.length === 1 ? 'thought' : 'thoughts'} saved on this device, waiting to send.`;
  const pending = local.find(item => item.id === selected) || local.at(-1);
  current = jobs.find(job => job.id === selected) || jobs.at(-1);
  const card = $('confirmation');
  $('undo').hidden = true; $('retry').hidden = true; $('previous').hidden = true; $('job-error').hidden = true;
  card.hidden = !pending && !current;
  if (pending) {
    $('action').textContent = 'Saved on this device'; $('result').textContent = 'Waiting to send…'; $('written-time').textContent = '';
  } else if (current) {
    const written = current.state === 'written';
    const actions = { new: 'Added', amend: 'Amended', link: 'Added + linked' };
    $('action').textContent = current.undone ? 'Undone' : written ? actions[current.action] : ({ queued: 'Queued', transcribing: 'Transcribing…', routing: 'Filing your thought…', failed: 'Couldn’t file this thought' }[current.state] || current.state);
    $('result').textContent = current.undone ? 'Your last write was undone.' : written ? current.line : current.state === 'failed' ? 'Your thought is saved. You can try again.' : 'Your thought is safely queued.';
    $('previous').hidden = !written || !current.previous || current.undone;
    $('previous').textContent = current.previous || '';
    $('job-error').hidden = !current.error || written;
    $('job-error').textContent = current.error || '';
    $('written-time').textContent = current.written_at ? new Date(current.written_at).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }) : '';
    $('written-time').dateTime = current.written_at || '';
    const last = jobs.filter(job => job.state === 'written').at(-1);
    $('undo').hidden = !written || current.undone || current.date !== today || current.id !== last?.id;
    $('retry').hidden = current.state !== 'failed';
  }
  const failed = jobs.filter(job => job.state === 'failed' && job.id !== current?.id);
  $('failures').hidden = failed.length === 0; $('failure-list').replaceChildren();
  for (const job of failed) {
    const li = document.createElement('li');
    const text = document.createElement('p'); text.textContent = `Capture from ${new Date(job.captured_at).toLocaleString()}: ${job.error}`;
    const retry = document.createElement('button'); retry.className = 'quiet'; retry.textContent = 'Try again'; retry.onclick = () => retryJob(job.id).catch(error => notice(error.message));
    li.append(text, retry); $('failure-list').append(li);
  }
}
async function status() {
  const events = statusEvents;
  const response = await fetch('/v1/status', { cache: 'no-store' });
  if (!response.ok) throw new Error('Could not read capture status.');
  const value = await response.json();
  // A status event that arrived while this response was in flight is at least
  // as new, and later changes send further events: a slow response must not
  // put an older state back on screen.
  if (events === statusEvents) await acceptStatus(value);
}
async function acceptStatus(value) {
  jobs = value.jobs; today = value.today;
  // A capture submitted from another device supersedes an older local card.
  const chosen = jobs.find(job => job.id === selected);
  if ((!chosen || jobs.at(-1)?.sequence > chosen.sequence) && !(await queued()).some(item => item.id === selected)) selected = jobs.at(-1)?.id || selected;
  await render();
}
async function sync() {
  if (sending) return;
  sending = true;
  try {
    // A job the server already reported through a status event is newer than the 202 receipt.
    await flush(async receipt => { if (!jobs.some(job => job.id === receipt.id)) { jobs.push(receipt); jobs.sort((a, b) => a.sequence - b.sequence); } await render(); });
    notice(); await status();
  } catch (error) { if ((await queued()).length) notice(error.message); }
  finally { sending = false; await render(); }
}
async function capture(value) {
  const item = { id: crypto.randomUUID(), captured_at: new Date().toISOString(), ...value };
  await enqueue(item); // Transaction completes before input clears or sending starts.
  selected = item.id; await setMeta('selected', selected); notice(); await render();
  if (registration?.sync) { try { await registration.sync.register('scribble-outbox'); } catch { /* opening/online fallback */ } }
  void sync();
}
$('capture').addEventListener('submit', async event => {
  event.preventDefault(); const text = $('thought').value.trim(); if (!text) return;
  if (new TextEncoder().encode(text).length > 16384) { notice('Please keep each thought under 16 KiB.'); return; }
  $('send').disabled = true;
  try { await capture({ text }); $('thought').value = ''; $('thought').focus(); }
  catch (error) { notice(`Could not save on this device: ${error.message}`); }
  finally { $('send').disabled = false; }
});
$('thought').addEventListener('keydown', event => { if ((event.ctrlKey || event.metaKey) && event.key === 'Enter') $('capture').requestSubmit(); });
// The server answers failures with {"error":{"code","message"}}.
async function errorMessage(response, fallback) {
  try { const message = (await response.json()).error?.message; if (typeof message === 'string' && message) return message; } catch { /* not JSON */ }
  return fallback;
}
async function mutate(id, action) {
  const response = await fetch(`/v1/thoughts/${id}/${action}`, { method: 'POST' });
  if (!response.ok) throw new Error(await errorMessage(response, 'Request failed.'));
  await status();
}
async function retryJob(id) { await mutate(id, 'retry'); selected = id; await setMeta('selected', id); await render(); }
$('undo').onclick = async () => { const id = current?.id; if (!id) return; $('undo').disabled = true; try { await mutate(id, 'undo'); notice(); } catch (error) { notice(error.message); } finally { $('undo').disabled = false; } };
$('retry').onclick = () => current && retryJob(current.id).catch(error => notice(error.message));
$('record').onclick = async () => {
  if (recording) { recording.stop(); return; }
  $('record').disabled = true;
  try {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    const mimeType = ['audio/webm;codecs=opus', 'audio/mp4', 'audio/ogg;codecs=opus'].find(type => MediaRecorder.isTypeSupported(type));
    const recorder = new MediaRecorder(stream, mimeType ? { mimeType } : {}); let chunks = [], size = 0;
    recording = recorder;
    recorder.ondataavailable = event => { if (event.data.size) { chunks.push(event.data); size += event.data.size; if (size > 24 * 1024 * 1024 && recorder.state === 'recording') recorder.stop(); } };
    recorder.onerror = () => { notice('Microphone recording failed. Please try again.'); if (recorder.state !== 'inactive') recorder.stop(); };
    recorder.onstop = async () => {
      clearTimeout(recordTimer); stream.getTracks().forEach(track => track.stop()); recording = null;
      $('record').setAttribute('aria-pressed', 'false'); $('record').textContent = '● Record a thought'; $('hint').textContent = 'Just capture. Obsidian keeps the rest.';
      if (!chunks.length) return;
      const audio = new Blob(chunks, { type: recorder.mimeType || chunks[0].type }); chunks = [];
      if (audio.size > 25 * 1024 * 1024) { notice('Recording was too large. Please record a shorter thought.'); return; }
      try { await capture({ audio }); } catch (error) { notice(`Could not save recording: ${error.message}`); }
    };
    recorder.start(1000); $('record').setAttribute('aria-pressed', 'true'); $('record').textContent = '■ Stop & scribble'; $('hint').textContent = 'Recording… tap stop to capture. Up to 2 minutes.'; notice();
    recordTimer = setTimeout(() => { if (recorder.state === 'recording') recorder.stop(); }, 120000);
  } catch (error) { notice(`Microphone unavailable: ${error.message}`); }
  finally { $('record').disabled = false; }
};
async function start() {
  selected = await getMeta('selected');
  if (!navigator.mediaDevices?.getUserMedia || !window.MediaRecorder) { $('record').disabled = true; $('record').title = 'Recording needs a supported browser over HTTPS.'; }
  if ('serviceWorker' in navigator) {
    try { registration = await navigator.serviceWorker.register('/sw.js', { type: 'module' }); } catch (error) { notice(`Offline setup failed: ${error.message}`); }
    navigator.serviceWorker.addEventListener('message', () => { void sync(); });
  }
  const events = new EventSource('/v1/events');
  events.onopen = () => { $('connection').textContent = 'Connected'; $('connection').className = 'online'; void sync(); };
  events.onerror = () => { $('connection').textContent = 'Offline · saved here'; $('connection').className = ''; };
  events.addEventListener('status', event => { statusEvents++; acceptStatus(JSON.parse(event.data)).catch(error => notice(error.message)); });
  window.addEventListener('online', () => { void sync(); });
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') void sync(); });
  // Online may mean Wi-Fi is connected while the tailnet service is restarting.
  setInterval(() => { if (document.visibilityState === 'visible') void sync(); }, 15000);
  await render(); await sync();
}
start().catch(error => notice(`Local capture storage is unavailable: ${error.message}`));
