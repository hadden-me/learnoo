import { test, expect } from '@playwright/test';
// Temporary: proves CI uploads browser results on failure. Removed before merge.
test('CI failure probe', async ({ page }) => {
  await page.goto('/lesson');
  await page.screenshot({ path: 'test-results/ci-probe.png' });
  expect('intentional').toBe('browser failure probe');
});
