import { test, expect } from '@playwright/test';
import { spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

// The scribble CLI against the service the Go fixture started with
// `scribble serve --root $SCRIBBLE_E2E_DIR/root`. Every command is given that
// --root, so it reaches <root>/scribble.sock and no other service. The CLI runs
// with this process's environment, which has neither the fake `claude` on its
// PATH nor the Jev key: the service has both.
//
// Playwright runs spec files in name order with one worker, so this file runs
// after capture.spec.js. Its last test stops the service.
const dir = process.env.SCRIBBLE_E2E_DIR;
const root = join(dir, 'root');
const socket = join(root, 'scribble.sock');
const bin = resolve('bin/scribble');
const scribble = (...args) => {
  const r = spawnSync(bin, [...args, '--root', root], { encoding: 'utf8', timeout: 60000 });
  return { code: r.status, out: r.stdout, err: r.stderr };
};
const json = r => JSON.parse(r.out);
const failed = 'Couldn’t file this thought';

test.describe.serial('scribble CLI against the running service', () => {
  test('the CLI environment is not the service environment', () => {
    expect(process.env.SCRIBBLE_FAKE_JEV_KEY).toBeUndefined();
    expect(existsSync(socket)).toBe(true);
  });

  test('status reports health, a disabled tailnet, today and the last write', async ({ request }) => {
    const text = scribble('status');
    expect(text.code).toBe(0);
    expect(text.out).toMatch(/^Health: ok$/m);
    expect(text.out).toMatch(/^Tailnet: disabled$/m);
    expect(text.out).toMatch(/^Today: \d{4}-\d\d-\d\d$/m);
    expect(text.out).toMatch(/^Pending: \d+$/m);
    expect(text.out).toMatch(/^Failed: \d+$/m);
    expect(text.out).toMatch(/^Last write: /m);
    const data = json(scribble('status', '--json'));
    expect(data.health.status).toBe('ok');
    expect(data.health.tailnet.state).toBe('disabled');
    // The CLI reports what the public API reports.
    const api = await (await request.get('/v1/status')).json();
    expect(data.status.today).toBe(api.today);
    expect(data.status.jobs.map(j => j.id)).toEqual(api.jobs.map(j => j.id));
  });

  test('retry and undo of a job show in the PWA and the API', async ({ page, request }) => {
    await page.goto('/');
    // The fixture's Jev rejects this thought for every automatic attempt and
    // accepts it once it is retried by hand.
    const id = randomUUID();
    expect((await request.post('/v1/thoughts', { data: { id, captured_at: new Date().toISOString(), text: 'fail until retried by the CLI' } })).status()).toBe(202);
    await expect(page.locator('#action')).toHaveText(failed);
    await expect.poll(async () => (await (await request.get(`/v1/thoughts/${id}`)).json()).state).toBe('failed');

    // An unknown id is not_found (exit 4).
    const unknown = scribble('retry', randomUUID(), '--json');
    expect(unknown.code).toBe(4);
    expect(JSON.parse(unknown.err).error.code).toBe('not_found');

    const retry = scribble('retry', id, '--json');
    expect(retry.code).toBe(0);
    expect(json(retry)).toEqual({ id, state: 'queued' });
    await expect.poll(async () => (await (await request.get(`/v1/thoughts/${id}`)).json()).state).toBe('written');
    await expect(page.locator('#action')).toHaveText('Added');
    await expect(page.locator('#result')).toContainText('fail until retried by the CLI');
    // A job that has not failed is a conflict (exit 5).
    const again = scribble('retry', id, '--json');
    expect(again.code).toBe(5);
    expect(JSON.parse(again.err).error.code).toBe('conflict');

    const undo = scribble('undo', id);
    expect(undo.code).toBe(0);
    expect(undo.out).toBe(`${id}: undone\n`);
    expect((await (await request.get(`/v1/thoughts/${id}`)).json()).undone).toBe(true);
    await expect(page.locator('#action')).toHaveText('Undone');
    // Undoing the job again changes nothing, as in the PWA.
    const twice = scribble('undo', id, '--json');
    expect(twice.code).toBe(0);
    expect(json(twice)).toEqual({ id, state: 'undone' });
    // An unknown job is not_found (exit 4).
    expect(scribble('undo', randomUUID()).code).toBe(4);
  });

  test('config shows what the service loaded, not the file on disk', () => {
    const loaded = json(scribble('config', '--json'));
    expect(loaded.version).toBe(1);
    expect(loaded.vault).toBe(join(dir, 'vault'));
    expect(loaded.listen).toMatchObject({ web: '127.0.0.1:18090', tailnet: '' });
    expect(loaded.log_level).toBe('info');
    expect(loaded.paths.root).toBe(root);
    expect(loaded.paths.socket).toBe(socket);
    const text = scribble('config');
    expect(text.code).toBe(0);
    expect(text.out).toContain('loaded by the running service');
    expect(text.out).toContain('web = "127.0.0.1:18090"');

    const file = join(root, 'config.toml');
    const original = readFileSync(file, 'utf8');
    try {
      const edited = original.replace(/^vault = /m, 'log_level = "debug"\nvault = ');
      expect(edited).not.toBe(original);
      writeFileSync(file, edited, 'utf8');
      expect(json(scribble('config', '--json')).log_level).toBe('info');
    } finally {
      writeFileSync(file, original, 'utf8');
    }
  });

  test('doctor runs its checks in the service environment', () => {
    const report = scribble('doctor', '--json');
    expect(report.code).toBe(0);
    const data = json(report);
    expect(data.source).toBe('service');
    expect(data.ok).toBe(true);
    const byName = Object.fromEntries(data.checks.map(c => [c.name, c]));
    expect(Object.keys(byName)).toEqual(['vault', 'daily-notes.json', 'claude', 'jev-key', 'whisper', 'tailnet']);
    // `claude` and the Jev key exist only in the service's environment.
    expect(byName.claude).toMatchObject({ status: 'ok' });
    expect(byName.claude.detail).toMatch(/^found at .*claude$/);
    expect(byName['jev-key'].status).toBe('ok');
    expect(byName.whisper.status).toBe('ok');
    expect(byName.tailnet).toMatchObject({ status: 'ok', detail: expect.stringContaining('disabled') });
    const text = scribble('doctor');
    expect(text.code).toBe(0);
    expect(text.out).toContain('Doctor (service)');
    expect(text.out).toContain('All checks passed.');
  });

  test('stop shuts the service down and returns once it has gone', async ({ request }) => {
    const pid = Number(readFileSync(join(dir, 'service.pid'), 'utf8'));
    process.kill(pid, 0); // still running
    const stop = scribble('stop');
    expect(stop.code).toBe(0);
    expect(stop.out).toContain('stopped');

    // stop returns after the service removed its socket as its last act.
    expect(existsSync(socket)).toBe(false);
    await expect.poll(() => existsSync(join(dir, 'service.exit'))).toBe(true);
    expect(readFileSync(join(dir, 'service.exit'), 'utf8')).toBe('0');
    expect(() => process.kill(pid, 0)).toThrow(/ESRCH/);
    // listen.web no longer answers.
    await expect(request.get('/v1/health')).rejects.toThrow(/ECONNREFUSED/);
    // Nothing answers on the socket any more: the commands cannot reach a
    // service, and doctor falls back to local checks and says so.
    const status = scribble('status', '--json');
    expect(status.code).toBe(5);
    expect(JSON.parse(status.err).error.code).toBe('unavailable');
    const doctor = scribble('doctor', '--json');
    expect(json(doctor).source).toBe('local');
  });
});
