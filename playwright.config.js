import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { defineConfig, devices } from '@playwright/test';

// The Go fixture runs `scribble serve --root $SCRIBBLE_E2E_DIR/root`, and the
// specs run the scribble CLI against that same root. The first evaluation of
// this file (Playwright's main process) picks the directory; the worker
// processes inherit it from the environment instead of picking another.
process.env.SCRIBBLE_E2E_DIR ||= mkdtempSync(join(tmpdir(), 'scribble-e2e-'));

export default defineConfig({
  testDir: './tests/e2e',
  testMatch: '**/*.spec.js',
  fullyParallel: false,
  workers: 1,
  timeout: 30000,
  expect: { timeout: 10000 },
  use: { baseURL: 'http://127.0.0.1:18090', trace: 'retain-on-failure', launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] } },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: 'go build -o bin/scribble ./cmd/scribble && SCRIBBLE_E2E=1 go test ./tests/e2e -run TestFixtureServer -count=1 -v -timeout=15m',
    url: 'http://127.0.0.1:18090/v1/health',
    reuseExistingServer: false,
    // The service's JSON log shows in the run output, which explains a failure.
    stdout: 'pipe',
    stderr: 'pipe',
    timeout: 120000
  }
});
