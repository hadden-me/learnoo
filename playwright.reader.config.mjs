import { defineConfig } from '@playwright/test';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
// Reader browser tests (LRN-008). Separate from playwright.config.mjs because
// they need a disposable PostgreSQL database: the web server is a Go test
// helper (internal/server TestBrowserReaderServer) that seeds TEST lessons in
// an isolated schema and drops it on shutdown.
if (!process.env.LEARNOO_TEST_DATABASE_URL) throw new Error('Set LEARNOO_TEST_DATABASE_URL to a disposable PostgreSQL database (see docs/DEVELOPMENT.md)');
const port = Number(process.env.LEARNOO_READER_TEST_PORT || 18280);
if (!Number.isInteger(port) || port < 1024 || port > 65535) throw new Error('LEARNOO_READER_TEST_PORT must be an integer from 1024 to 65535');
const baseURL = `http://127.0.0.1:${port}`;
// Absolute: the Go helper runs in its package directory, and Playwright
// empties test-results. The helper deletes the file on shutdown.
export const tokenFile = join(tmpdir(), `learnoo-reader-invitation-${port}.txt`);
export default defineConfig({
  testDir: './tests/browser-reader', timeout: 60000, fullyParallel: false, workers: 1,
  use: { baseURL, browserName: 'chromium' },
  webServer: {
    command: "go test ./internal/server -run '^TestBrowserReaderServer$' -count=1 -timeout 0",
    env: { LEARNOO_BROWSER_READER_ADDR: `127.0.0.1:${port}`, LEARNOO_BROWSER_READER_TOKEN_FILE: tokenFile },
    url: `${baseURL}/login`, reuseExistingServer: false, timeout: 180000,
    gracefulShutdown: { signal: 'SIGTERM', timeout: 10000 },
  },
  reporter: [['list']],
});
