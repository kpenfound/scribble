import { test, expect } from '@playwright/test';
import { randomUUID } from 'node:crypto';
const submit = async (page, text) => { await page.getByLabel("What's on your mind?").fill(text); await page.getByRole('button', { name: 'Scribble', exact: false }).filter({ hasText: /^Scribble/ }).click(); };
const waitWritten = async page => { await expect(page.locator('#action')).toHaveText(/^(Added|Amended|Added \+ linked)$/); };

test.describe.serial('capture lifecycle with the real binary', () => {
  test('text capture, idempotency, and undo', async ({ page, request }) => {
    await page.goto('/'); await submit(page, 'A raw thought — punctuation stays.'); await waitWritten(page);
    await expect(page.locator('#result')).toContainText('A raw thought — punctuation stays.');
    await expect(page.locator('#result')).toContainText(/\^scr-[a-z0-9]{4}/);
    const data = { id: randomUUID(), captured_at: new Date().toISOString(), text: 'Only once' };
    const first = await request.post('/v1/thoughts', { data }); const second = await request.post('/v1/thoughts', { data });
    expect(first.status()).toBe(202); expect(second.status()).toBe(202);
    expect((await first.json()).id).toBe((await second.json()).id);
    await expect(page.locator('#result')).toContainText('Only once');
    await page.getByRole('button', { name: 'Undo', exact: true }).click(); await expect(page.locator('#action')).toHaveText('Undone');
  });
  test('offline outbox survives reload and sends FIFO on reconnect', async ({ page, context }) => {
    await page.goto('/'); await page.evaluate(async () => { await navigator.serviceWorker.ready; });
    await expect.poll(() => page.evaluate(() => !!navigator.serviceWorker.controller)).toBe(true);
    await context.setOffline(true);
    await submit(page, 'Offline first thought');
    await expect(page.locator('#outbox')).toContainText('1 thought saved');
    await submit(page, 'Offline second thought');
    await expect(page.locator('#outbox')).toContainText('2 thoughts saved');
    await page.reload(); await expect(page.locator('#outbox')).toContainText('2 thoughts saved');
    await context.setOffline(false);
    await expect(page.locator('#result')).toContainText('Offline second thought'); await waitWritten(page);
    await expect(page.locator('#outbox')).toBeHidden();
  });
  test('new tag and correction use the agentcore Claude fixture', async ({ page }) => {
    await page.goto('/'); await submit(page, 'new tag for a conference talk'); await waitWritten(page);
    await expect(page.locator('#result')).toContainText('#idea/talk');
    const firstLine = await page.locator('#result').textContent(); const block = firstLine.match(/\^scr-\w{4}/)[0];
    await submit(page, 'correct that conference talk'); await expect(page.locator('#action')).toHaveText('Amended');
    await expect(page.locator('#previous')).toHaveText(firstLine);
    await expect(page.locator('#result')).toContainText('Corrected conference talk');
    await expect(page.locator('#result')).toContainText(block);
    await page.getByRole('button', { name: 'Undo', exact: true }).click(); await expect(page.locator('#action')).toHaveText('Undone');
  });
  test('past thoughts link and lower-confidence same-day targets link', async ({ page }) => {
    // The page keeps showing the previous job until the new one is accepted, so
    // wait for the new job's line before asserting how it was written.
    await page.goto('/'); await submit(page, 'follow yesterday with this detail');
    await expect(page.locator('#result')).toContainText('#^scr-past]]'); await expect(page.locator('#action')).toHaveText('Added + linked');
    await submit(page, 'weak follow on the last thought');
    await expect(page.locator('#result')).toContainText('weak follow'); await expect(page.locator('#action')).toHaveText('Added + linked');
  });
  test('terminal provider failures are visible and the queue continues', async ({ page }) => {
    await page.goto('/'); await submit(page, 'fail routing fixture');
    await expect(page.locator('#action')).toHaveText('Couldn’t file this thought');
    await expect(page.locator('#job-error')).toContainText('Jev unavailable');
    await submit(page, 'A thought after the failed one'); await waitWritten(page);
    await expect(page.locator('#result')).toContainText('A thought after the failed one');
    await expect(page.locator('#failures')).toBeVisible();
  });
  test('voice uploads follow transcription and expose transcription failures', async ({ page, request }) => {
    await page.goto('/');
    const response = await request.post('/v1/thoughts', { multipart: {
      id: randomUUID(), captured_at: new Date().toISOString(),
      audio: { name: 'fixture.wav', mimeType: 'audio/wav', buffer: Buffer.from('RIFF fixture audio') }
    } });
    expect(response.status()).toBe(202); await expect(page.locator('#result')).toContainText('Voice fixture thought');
    const failed = await request.post('/v1/thoughts', { multipart: {
      id: randomUUID(), captured_at: new Date().toISOString(),
      audio: { name: 'failure.wav', mimeType: 'audio/wav', buffer: Buffer.from('unavailable fixture') }
    } });
    expect(failed.status()).toBe(202); await expect(page.locator('#action')).toHaveText('Couldn’t file this thought');
    await expect(page.locator('#job-error')).toContainText('transcription returned HTTP 503');
  });
  test('mobile microphone capture uses MediaRecorder and the durable outbox', async ({ browser }) => {
    const context = await browser.newContext({ baseURL: 'http://127.0.0.1:18090', viewport: { width: 390, height: 844 }, permissions: ['microphone'] });
    const page = await context.newPage();
    // Synthetic microphone input avoids operating-system permission prompts;
    // the browser's real MediaRecorder still creates and uploads the audio.
    await page.addInitScript(() => {
      navigator.mediaDevices.getUserMedia = async () => {
        const audio = new AudioContext(); const oscillator = audio.createOscillator();
        const output = audio.createMediaStreamDestination(); oscillator.connect(output); oscillator.start();
        output.stream.getTracks()[0].addEventListener('ended', () => audio.close());
        return output.stream;
      };
    });
    await page.goto('/');
    await page.getByRole('button', { name: 'Record a thought' }).click();
    await expect(page.getByRole('button', { name: 'Stop & scribble' })).toBeVisible();
    await page.waitForTimeout(1200); await page.getByRole('button', { name: 'Stop & scribble' }).click();
    await expect(page.locator('#result')).toContainText('Voice fixture thought'); await waitWritten(page);
    await expect(page.locator('body')).toHaveJSProperty('scrollWidth', 390);
    await page.screenshot({ path: 'test-results/mobile-capture.png', fullPage: true }); await context.close();
  });
  test('a refused undo or retry shows the server message', async ({ page, request }) => {
    // The server's own refusals: a request aimed at a job it cannot act on is
    // answered with the error envelope, and the PWA shows its message.
    const older = randomUUID();
    expect((await request.post('/v1/thoughts', { data: { id: older, captured_at: new Date().toISOString(), text: 'An older written thought' } })).status()).toBe(202);
    await expect.poll(async () => (await (await request.get(`/v1/thoughts/${older}`)).json()).state).toBe('written');
    await page.goto('/'); await submit(page, 'The latest written thought');
    // The page first shows the older written job; wait for the new one before undoing.
    await expect(page.locator('#result')).toContainText('The latest written thought'); await waitWritten(page);
    const refuse = async action => {
      const pattern = new RegExp(`/v1/thoughts/[^/]+/${action}$`);
      await page.route(pattern, route => route.continue({ url: new URL(`/v1/thoughts/${older}/${action}`, page.url()).href }));
      return pattern;
    };
    const undoRefusal = await refuse('undo');
    await page.getByRole('button', { name: 'Undo', exact: true }).click();
    await expect(page.locator('#notice')).toBeVisible();
    await expect(page.locator('#notice')).toHaveText('undo only applies to the most recent completed write');
    await expect(page.locator('#action')).not.toHaveText('Undone');
    await page.unroute(undoRefusal);
    await submit(page, 'fail routing fixture'); await expect(page.locator('#action')).toHaveText('Couldn’t file this thought');
    await refuse('retry');
    await page.locator('#retry').click();
    await expect(page.locator('#notice')).toHaveText('only failed jobs can be retried');
    await expect(page.locator('#notice')).not.toContainText('[object Object]');
  });
  test('a capture the server does not accept stays in the outbox with the server message', async ({ page }) => {
    await page.goto('/');
    // Blank text is a server-side validation failure the PWA cannot predict.
    await page.route('**/v1/thoughts', route => route.request().method() === 'POST' ? route.continue({ postData: JSON.stringify({ id: randomUUID(), captured_at: new Date().toISOString(), text: ' ' }) }) : route.continue());
    await submit(page, 'Held back by the server');
    await expect(page.locator('#notice')).toContainText('text must be between 1 and 16384 bytes and contain no NUL');
    await expect(page.locator('#notice')).toContainText('Your capture is still saved on this device.');
    await expect(page.locator('#notice')).not.toContainText('[object Object]');
    await expect(page.locator('#outbox')).toContainText('1 thought saved');
    // Once the server accepts it, the same item sends and leaves the outbox.
    await page.unroute('**/v1/thoughts');
    await page.evaluate(() => window.dispatchEvent(new Event('online')));
    await expect(page.locator('#result')).toContainText('Held back by the server'); await waitWritten(page);
    await expect(page.locator('#outbox')).toBeHidden();
  });
  test('a slow status response does not replace a newer status event', async ({ page }) => {
    await page.goto('/'); await expect(page.locator('#connection')).toHaveText('Connected');
    // The status request the page makes after the server accepts the capture is
    // held until the job has been written, then answered with the job as it was
    // before the write, the way a slow response can be. The fixture's Jev takes
    // a moment over "slow routing", so the request starts before the write.
    const held = [];
    await page.route('**/v1/status', route => { held.push(route); });
    await submit(page, 'slow routing thought');
    await expect.poll(() => held.length).toBeGreaterThan(0);
    // The status event delivers the written job while the request is held.
    await expect(page.locator('#result')).toContainText('slow routing thought');
    await waitWritten(page);
    for (const route of held.splice(0)) {
      const response = await route.fetch(); const body = await response.json();
      body.jobs = body.jobs.map(job => ({ ...job, state: 'queued', line: '', action: '', written_at: '', undone: false }));
      await route.fulfill({ response, json: body });
    }
    await page.waitForTimeout(500);
    await expect(page.locator('#result')).toContainText('slow routing thought');
    await waitWritten(page);
    await page.unroute('**/v1/status');
  });
});
