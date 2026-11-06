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
    const first = await request.post('/thoughts', { data }); const second = await request.post('/thoughts', { data });
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
    await page.goto('/'); await submit(page, 'follow yesterday with this detail'); await expect(page.locator('#action')).toHaveText('Added + linked');
    await expect(page.locator('#result')).toContainText('#^scr-past]]');
    await submit(page, 'weak follow on the last thought'); await expect(page.locator('#action')).toHaveText('Added + linked');
    await expect(page.locator('#result')).toContainText('weak follow');
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
    const response = await request.post('/thoughts', { multipart: {
      id: randomUUID(), captured_at: new Date().toISOString(),
      audio: { name: 'fixture.wav', mimeType: 'audio/wav', buffer: Buffer.from('RIFF fixture audio') }
    } });
    expect(response.status()).toBe(202); await expect(page.locator('#result')).toContainText('Voice fixture thought');
    const failed = await request.post('/thoughts', { multipart: {
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
});
