import { defineConfig, devices } from '@playwright/test';
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
    url: 'http://127.0.0.1:18090/healthz',
    reuseExistingServer: false,
    timeout: 120000
  }
});
