import { test, expect } from '@playwright/test';
import { apiCourse, apiSession, author, closeFixture, saveState, setup, signIn } from './course-fixture.mjs';

// Retires the fixture's only chapter (test-chapter), which the lesson pickers
// of studio.spec.mjs and the reader tests use, so this file must run last:
// Playwright runs spec files in file-name order with one worker
// (playwright.reader.config.mjs), hence the "zz-" prefix. The fixture server
// has no API to create a second chapter (chapters are added by the operator
// CLI), and the fixture itself is not changed here.
test('a module chapter retired after the last save is flagged, refused by the save, and removable', async ({ page, browser, baseURL }) => {
  const f = await setup(browser, baseURL);
  const au = await apiSession(browser, baseURL, author);
  const current = await f.current();
  const { course, revision } = await apiCourse(au, f.subject.id, {
    title: 'TEST Cours au chapitre retiré',
    assignmentIds: [current.assignmentId],
    modules: [{ key: null, title: 'TEST Module chapitré', chapterId: f.chapter.id, steps: [] }],
  });
  await f.ad.call('POST', `/api/admin/curriculum/chapters/${f.chapter.id}/retire`, {});

  await signIn(page, author, `/studio/courses/${course.id}`);
  await expect(page.getByLabel('Chapitre officiel (facultatif)')).toHaveValue(String(f.chapter.id));
  await expect(page.getByText('Le prochain enregistrement sera refusé : ce chapitre officiel a été retiré du programme')).toBeVisible();

  await page.getByLabel('Titre du module 1').fill('TEST Module chapitré, renommé');
  await page.getByRole('button', { name: 'Enregistrer', exact: true }).click();
  await expect(page.getByRole('alert').filter({ hasText: 'Classement refusé' })).toBeVisible();
  await expect(page.getByLabel('Titre du module 1')).toHaveValue('TEST Module chapitré, renommé');

  await page.getByRole('button', { name: 'Retirer le chapitre officiel du module 1 : TEST Module chapitré, renommé' }).click();
  await expect(page.getByLabel('Chapitre officiel (facultatif)')).toHaveValue('');
  await expect(page.getByText('Le prochain enregistrement sera refusé')).toHaveCount(0);
  await expect(page.getByLabel('Titre du module 1')).toBeFocused();
  await page.getByRole('button', { name: 'Enregistrer', exact: true }).click();
  await expect(saveState(page)).toHaveText(/Enregistré sur le serveur/);

  const saved = await au.call('GET', `/api/course-revisions/${revision.id}`);
  expect(saved.modules.map((m) => [m.title, m.chapterId])).toEqual([['TEST Module chapitré, renommé', null]]);
  await au.ctx.close();
});

test.afterAll(closeFixture);
