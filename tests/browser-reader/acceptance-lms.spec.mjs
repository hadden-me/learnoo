import { test, expect } from '@playwright/test';
import { apiSession, noOverflow, password, watch } from './course-fixture.mjs';
import { messages, pattern } from './acceptance-messages.mjs';

// LMS-B12 acceptance (docs/LMS_ACCEPTANCE.md): the central LMS journey as one
// continuous flow through the real interface, against the Go application and
// PostgreSQL (internal/server TestBrowserReaderServer), once in the French
// and once in the Arabic interface:
//   1. an administrator sets up a level, a section, a current edition and its
//      assignment of the TEST subject (LMS-A09);
//   2. a teacher creates a course for that subject and level, organizes a
//      module and steps from their own published lessons, saves and submits
//      (LMS-A02, A04);
//   3. the teacher, also a reviewer of the subject, is refused the review of
//      their own course in the Studio and by the server; an independent
//      reviewer approves it (LMS-A04);
//   4. a publisher publishes it (LMS-A04);
//   5. a signed-out visitor finds it from / → catalogue → overview, cannot
//      fetch a lesson, signs in as a student and reads it with previous/next
//      at phone width (LMS-A01, A05, A10);
//   6. an administrator withdraws an included lesson's revision: the student
//      sees the unavailable step and navigation stops at it; a newer
//      revision is published, the teacher creates an amendment, the reviewer
//      sees only that step replaced and approves, the publisher publishes,
//      and the step reads again (LMS-A06, A07).
// The slice specs prove the other steps of each journey (the table in
// docs/LMS_ACCEPTANCE.md names them); this spec does not repeat them.
// The lessons' own workflow (submission, lesson review, lesson publication)
// is driven through the API here: editorial.spec.mjs and studio.spec.mjs
// prove it in the interface. Accounts are the fixture's simulated TEST users
// (test-acc-teacher, test-acc-student-fr/-ar are used by no other spec);
// every curriculum entry, course and lesson created here is labelled TEST.
// Desktop Chromium; 360 px is viewport emulation, not a physical phone.

const account = (name) => ({ email: `${name}@example.test`, password });
const teacher = account('test-acc-teacher');
const reviewer = account('test-reviewer');
const publisher = account('test-publisher');
const admin = account('test-admin');

const run = Date.now().toString(36);
// A missing control fails at its step rather than at the test timeout.
test.use({ actionTimeout: 20000 });
const desktop = { width: 1280, height: 900 };
const phone = { width: 360, height: 780 };

/**
 * What a journey must undo even when it fails or times out, released by
 * test.afterEach (which runs with its own timeout): every browser and API
 * context it opened, and the teacher's temporary reviewer grant (a failed
 * revoke fails the test).
 */
const opened = [];
let temporaryGrant = null;
const track = (context) => (opened.push(context), context);

test.afterEach(async ({ browser, baseURL }) => {
  for (const context of opened.splice(0)) await context.close().catch((e) => console.warn(`acceptance-lms: closing a context failed: ${e}`));
  const grant = temporaryGrant;
  temporaryGrant = null;
  if (!grant) return;
  // Revoking is idempotent (an absent grant changes nothing). A failure is thrown after the
  // contexts are closed, so Playwright reports it alongside any error of the test itself and a
  // passing journey can never leave the grant behind unnoticed.
  const cleaner = await apiSession(browser, baseURL, admin);
  try {
    await cleaner.call('POST', `/api/admin/users/${grant.userId}/revoke`, { permission: 'reviewer', subjectId: grant.subjectId });
  } catch (e) {
    throw new Error(`acceptance-lms: could not revoke the teacher's reviewer grant on subject ${grant.subjectId}: ${e.message}`);
  } finally {
    await cleaner.ctx.close();
  }
});

/** A fresh context signed in as `user` with the interface in `lang`, opened at `path`. */
async function signedIn(browser, baseURL, user, lang, path, viewport = desktop) {
  const { r } = messages(lang);
  const context = track(await browser.newContext({ baseURL, viewport }));
  const page = await context.newPage();
  const problems = watch(page, baseURL);
  await page.goto(`/login?next=${encodeURIComponent(path)}&lang=${lang}`);
  await expect(page.locator('html')).toHaveAttribute('lang', lang);
  await page.getByLabel(r('field.email')).fill(user.email);
  await page.getByLabel(r('field.password')).fill(user.password);
  await page.getByRole('button', { name: r('login.submit') }).click();
  await expect(page).not.toHaveURL(/\/login/);
  return { context, page, problems };
}

/** A TEST lesson document with a formula, in French (the content language policy). */
function lessonDoc(title, body) {
  return {
    schemaVersion: 1, title, language: 'fr', status: 'unreviewed', objective: 'TEST Objectif de la leçon.',
    blocks: [{ type: 'paragraph', children: [{ type: 'text', text: `${body} ` }, { type: 'math', text: 'z^2 = -1' }] }],
  };
}

/** Lesson workflow through the API: the teacher writes and submits, the reviewer approves, the publisher publishes. */
async function publishLessonRevision(t, rv, pu, created) {
  const sub = await t.call('POST', `/api/revisions/${created.id}/submit`, { expectedVersion: created.version });
  await rv.call('POST', `/api/review/revisions/${sub.id}/approve`, {});
  await pu.call('POST', `/api/revisions/${sub.id}/publish`, {});
  return sub;
}

for (const lang of ['fr', 'ar']) {
  test(`central LMS journey: curriculum, course authoring, independent review, publication, discovery, reading, withdrawal and amendment (${lang})`, async ({ browser, baseURL }) => {
    test.setTimeout(300000);
    const { s, r } = messages(lang);
    const student = account(`test-acc-student-${lang}`);
    const t = await apiSession(browser, baseURL, teacher);
    track(t.ctx);
    const rv = await apiSession(browser, baseURL, reviewer);
    track(rv.ctx);
    const pu = await apiSession(browser, baseURL, publisher);
    track(pu.ctx);
    const adm = await apiSession(browser, baseURL, admin);
    track(adm.ctx);
    const subject = (await t.call('GET', '/api/catalogue/subjects')).subjects.find((x) => x.code === 'test-math');
    // The teacher reviews the subject only during this journey, so that the self-review refusal is shown to them.
    // Recorded before the call, so that test.afterEach revokes it whatever happens next.
    temporaryGrant = { userId: t.me.id, subjectId: subject.id };
    await adm.call('POST', `/api/admin/users/${t.me.id}/grant`, { permission: 'reviewer', subjectId: subject.id });
    const chapter = (await t.call('GET', '/api/catalogue/subjects/test-math/chapters')).chapters.find((c) => c.code === 'test-chapter');
    const subjectName = lang === 'ar' ? subject.nameAr : subject.nameFr;

    // The teacher's own published lessons (lesson workflow through the API).
    const lessons = [];
    for (const [n, body] of [['A', `TEST Corps de la leçon A ${lang}.`], ['B', `TEST Corps de la leçon B ${lang}.`]]) {
      const title = `TEST Leçon ${n} acceptation ${lang} ${run}`;
      const created = await t.call('POST', '/api/lessons', { subjectId: subject.id, language: 'fr', content: lessonDoc(title, body) });
      await publishLessonRevision(t, rv, pu, created);
      const mine = (await t.call('GET', '/api/lessons/mine')).lessons.find((l) => l.id === created.lessonId);
      lessons.push({ title, body, lessonId: created.lessonId, publicId: mine.publicId });
    }
    const [lessonA, lessonB] = lessons;

    // 1. The administrator sets up a level, a section, a current edition and its assignment of the subject.
    const level = { code: `test-acc-${lang}-${run}`, nameFr: `TEST Lycée acceptation ${lang}`, nameAr: `TEST ثانوي قبول ${lang}` };
    const section = { code: `test-acc-sec-${lang}-${run}`, nameFr: `TEST Section acceptation ${lang}`, nameAr: `TEST شعبة قبول ${lang}` };
    const edition = { code: `test-acc-ed-${lang}-${run}`, labelFr: `TEST Programme acceptation ${lang}`, labelAr: `TEST برنامج قبول ${lang}` };
    const named = (x) => (lang === 'ar' ? x.nameAr : x.nameFr);
    const editionLabel = lang === 'ar' ? edition.labelAr : edition.labelFr;
    const ad = await signedIn(browser, baseURL, admin, lang, '/studio/admin/curriculum');
    await ad.page.getByText(s('admin.curriculum.newLevel'), { exact: true }).click();
    const levelForm = ad.page.getByRole('form', { name: s('admin.curriculum.newLevel') });
    await levelForm.getByLabel(s('admin.code')).fill(level.code);
    await levelForm.getByLabel(s('admin.nameFr')).fill(level.nameFr);
    await levelForm.getByLabel(s('admin.nameAr')).fill(level.nameAr);
    await levelForm.getByRole('button', { name: s('admin.curriculum.createLevel') }).click();
    await expect(ad.page.getByText(s('admin.curriculum.levelCreated', { code: level.code }))).toBeVisible();
    await ad.page.getByText(s('admin.curriculum.newSection', { code: level.code }), { exact: true }).click();
    const sectionForm = ad.page.getByRole('form', { name: s('admin.curriculum.newSection', { code: level.code }) });
    await sectionForm.getByLabel(s('admin.code')).fill(section.code);
    await sectionForm.getByLabel(s('admin.nameFr')).fill(section.nameFr);
    await sectionForm.getByLabel(s('admin.nameAr')).fill(section.nameAr);
    await sectionForm.getByRole('button', { name: s('admin.curriculum.createSection') }).click();
    await ad.page.getByRole('link', { name: s('admin.curriculum.sectionEditions', { name: section.code }) }).click();
    const editionForm = ad.page.getByRole('form', { name: s('admin.curriculum.newEdition') });
    await editionForm.getByLabel(s('admin.code')).fill(edition.code);
    await editionForm.getByLabel(s('admin.curriculum.labelFr')).fill(edition.labelFr);
    await editionForm.getByLabel(s('admin.curriculum.labelAr')).fill(edition.labelAr);
    await editionForm.getByRole('button', { name: s('admin.curriculum.createEdition') }).click();
    await ad.page.getByLabel(s('admin.edition.subjectToAdd')).selectOption(String(subject.id));
    await ad.page.getByRole('button', { name: s('admin.edition.addSubject') }).click();
    await expect(ad.page.getByText(s('admin.edition.subjectAdded'))).toBeVisible();
    await ad.page.getByLabel(s('admin.edition.chapterToAdd', { code: 'test-math' })).selectOption(String(chapter.id));
    await ad.page.getByRole('button', { name: s('admin.edition.addChapter') }).click();
    await ad.page.getByRole('button', { name: s('admin.edition.saveChapters') }).click();
    await expect(ad.page.getByText(s('admin.edition.chaptersSaved', { code: 'test-math' }))).toBeVisible();
    await ad.page.getByRole('button', { name: s('admin.edition.makeCurrent') }).click();
    await ad.page.getByRole('dialog', { name: s('admin.edition.confirmMakeCurrent') }).getByRole('button', { name: s('admin.edition.makeCurrent') }).click();
    await expect(ad.page.getByText(s('admin.edition.madeCurrent'))).toBeVisible();

    // 2. The teacher creates the course, places it, builds a module from their own published lessons, saves and submits.
    const courseTitle = `TEST Cours acceptation ${lang} ${run}`;
    const moduleTitle = `TEST Module acceptation ${lang}`;
    const te = await signedIn(browser, baseURL, teacher, lang, '/studio/courses/new');
    await te.page.getByLabel(s('courses.new.subject')).selectOption(String(subject.id));
    await te.page.locator('input[name="course-language"][value="fr"]').check();
    await te.page.getByRole('button', { name: s('courses.new.create') }).click();
    await expect(te.page).toHaveURL(/\/studio\/courses\/\d+$/);
    const courseId = Number(new URL(te.page.url()).pathname.split('/').pop());
    await te.page.getByLabel(s('courses.field.title')).fill(courseTitle);
    await te.page.getByLabel(s('courses.editor.summaryLabel')).fill('TEST Un cours suivi de bout en bout.');
    await te.page.getByLabel(s('courses.editor.objectivesLabel')).fill('TEST Calculer avec les nombres complexes.');
    // The Studio names catalogue entries in the course's language (French here), whatever the interface.
    await te.page.getByLabel(s('courses.placement.level')).selectOption({ label: level.nameFr });
    await te.page.getByLabel(s('courses.placement.section'), { exact: true }).selectOption({ label: section.nameFr });
    await te.page.getByRole('button', { name: s('courses.placement.addCurrent', { name: edition.labelFr }) }).click();
    await te.page.getByLabel(s('courses.outline.newModule')).fill(moduleTitle);
    await te.page.keyboard.press('Enter');
    await te.page.getByLabel(s('courses.outline.chapter')).first().selectOption(String(chapter.id));
    for (const lesson of lessons) {
      await te.page.getByRole('button', { name: s('courses.outline.addLessonLabel', { index: 1, name: moduleTitle }) }).click();
      const dialog = te.page.getByRole('dialog', { name: s('courses.picker.addTitle', { title: moduleTitle }) });
      await dialog.getByRole('searchbox').fill(lesson.title);
      await dialog.getByRole('button', { name: s('courses.picker.searchButton') }).click();
      await dialog.getByRole('button', { name: new RegExp(`${lesson.title}.*${s('courses.yourLesson')}`) }).click();
      await expect(dialog.getByLabel(s('courses.picker.revisionLabel'))).toContainText(s('courses.picker.eligible'));
      await dialog.getByRole('button', { name: s('courses.picker.addStep') }).click();
      await expect(dialog).toHaveCount(0);
    }
    await te.page.getByRole('button', { name: s('courses.editor.save'), exact: true }).click();
    await expect(te.page.locator('.save-state')).toHaveText(pattern(s('courses.editor.savedAt')));
    await te.page.getByRole('button', { name: s('courses.editor.submit') }).click();
    await te.page.getByRole('dialog', { name: s('courses.editor.submitTitle', { number: 1 }) }).getByRole('button', { name: s('courses.editor.submitAction'), exact: true }).click();
    await expect(te.page.getByRole('heading', { name: s('courses.status.heading', { number: 1, status: s('shell.status.in_review') }) })).toBeVisible();

    // 3. Self-review is refused: the teacher reviews test-math, but authored this course revision.
    const firstRevision = (await t.call('GET', '/api/courses?mine=1')).courses.find((c) => c.id === courseId).latestRevision.id;
    await te.page.getByRole('navigation', { name: s('shell.nav') }).getByRole('link', { name: s('shell.space.review') }).click();
    const ownCard = te.page.getByRole('listitem').filter({ hasText: courseTitle });
    await expect(ownCard).toContainText(s('review.queue.decision.other'));
    await ownCard.getByRole('link', { name: courseTitle }).click();
    await expect(te.page.getByRole('heading', { name: s('review.course.notEligibleHeading') })).toBeVisible();
    await expect(te.page.getByText(s('courses.own.courseAuthor'))).toBeVisible();
    await expect(te.page.getByRole('button', { name: s('review.course.approveButton') })).toHaveCount(0);
    const csrf = (await (await te.page.request.get('/api/auth/session')).json()).csrfToken;
    const refused = await te.page.request.post(`/api/review/course-revisions/${firstRevision}/approve`, { data: {}, headers: { 'X-CSRF-Token': csrf } });
    expect(refused.status()).toBe(403);
    expect((await refused.json()).error.reasons.map((x) => x.reason)).toContain('course_author');

    // The independent reviewer of the subject approves.
    const re = await signedIn(browser, baseURL, reviewer, lang, '/studio/review');
    const card = re.page.getByRole('listitem').filter({ hasText: courseTitle });
    await expect(card).toContainText(s('review.queue.canDecide'));
    await card.getByRole('link', { name: courseTitle }).click();
    await expect(re.page.getByRole('heading', { name: courseTitle, level: 1 })).toBeVisible();
    await expect(re.page.locator('.review-step[data-change="added"]')).toHaveCount(2);
    await re.page.getByRole('button', { name: s('review.course.approveButton') }).click();
    await re.page.getByRole('dialog', { name: s('review.course.approveTitle', { number: 1 }) }).getByRole('button', { name: s('review.decision.approve'), exact: true }).click();
    await expect(re.page.getByText(s('review.course.approved', { title: courseTitle }))).toBeVisible();

    // 4. The publisher publishes it.
    const pb = await signedIn(browser, baseURL, publisher, lang, '/studio/publish');
    const queued = pb.page.getByRole('list', { name: s('review.coursePublish.list') }).getByRole('listitem').filter({ hasText: courseTitle });
    await expect(queued).toContainText(s('review.coursePublish.noPublished'));
    await queued.getByRole('link', { name: courseTitle }).click();
    await pb.page.getByRole('button', { name: s('review.coursePublish.button') }).click();
    await pb.page.getByRole('dialog', { name: s('review.coursePublish.dialogTitle', { title: courseTitle }) }).getByRole('button', { name: s('review.publish'), exact: true }).click();
    await expect(pb.page.getByRole('heading', { name: s('review.coursePublish.done') })).toBeVisible();

    // 5. A signed-out visitor discovers the course at phone width: / → catalogue → level/section → subject → overview.
    const visitor = track(await browser.newContext({ baseURL, viewport: phone }));
    const vp = await visitor.newPage();
    const visitorProblems = watch(vp, baseURL);
    await vp.goto(lang === 'ar' ? '/?lang=ar' : '/');
    await expect(vp.locator('html')).toHaveAttribute('dir', lang === 'ar' ? 'rtl' : 'ltr');
    await vp.getByRole('link', { name: r('pub.browse') }).click();
    await expect(vp.getByRole('heading', { level: 2, name: named(level) })).toBeVisible();
    await vp.getByRole('link', { name: named(section) }).click();
    await expect(vp).toHaveURL(new RegExp(`/catalogue/${level.code}/${section.code}`));
    await expect(vp.getByText(editionLabel).first()).toBeVisible();
    await vp.getByRole('link', { name: subjectName }).first().click();
    const coursesRegion = vp.getByRole('region', { name: r('pub.courses') });
    await coursesRegion.getByRole('link', { name: courseTitle }).click();
    await expect(vp).toHaveURL(/\/courses\/[0-9a-f-]{36}/);
    const coursePublicId = new URL(vp.url()).pathname.split('/').pop();
    await expect(vp.getByRole('heading', { level: 1, name: courseTitle })).toBeVisible();
    await expect(vp.getByText(lessonA.title).first()).toBeVisible();
    await expect(vp.getByText(lessonA.body)).toHaveCount(0);
    expect(await noOverflow(vp)).toBe(true);
    // Signed out, neither the lesson nor the course step can be fetched: both redirect to sign-in.
    for (const path of [`/learn/lessons/${lessonA.publicId}`, `/learn/courses/${coursePublicId}/lessons/${lessonA.publicId}`]) {
      const res = await vp.request.get(path, { maxRedirects: 0 });
      expect(res.status(), path).toBe(303);
      expect(res.headers()['location'], path).toMatch(/^\/login\?next=/);
      expect(await res.text()).not.toContain(lessonA.body);
    }
    // Sign in from the overview as a student: back on the course, then read with previous/next.
    await vp.getByRole('link', { name: r('pub.course.signIn') }).click();
    await vp.getByLabel(r('field.email')).fill(student.email);
    await vp.getByLabel(r('field.password')).fill(student.password);
    await vp.getByRole('button', { name: r('login.submit') }).click();
    await expect(vp).toHaveURL(new RegExp(`/learn/courses/${coursePublicId}$`));
    const outline = vp.getByRole('navigation', { name: r('course.outline') });
    await expect(outline.getByRole('link')).toHaveText([lessonA.title, lessonB.title]);
    const stepBURL = await outline.getByRole('link', { name: lessonB.title }).getAttribute('href');
    await vp.getByRole('link', { name: r('course.start') }).click();
    await expect(vp.getByText(lessonA.body)).toBeVisible();
    await expect(vp.locator('.math[data-rendered="true"] .katex').first()).toBeVisible();
    await expect(vp.getByText(r('step.position', 1, 2))).toBeVisible();
    const steps = vp.getByRole('navigation', { name: r('step.navLabel') });
    await steps.getByRole('link', { name: new RegExp(r('step.next')) }).click();
    await expect(vp.getByText(lessonB.body)).toBeVisible();
    await expect(vp.getByText(r('step.position', 2, 2))).toBeVisible();
    await steps.getByRole('link', { name: new RegExp(r('step.previous')) }).click();
    await expect(vp.getByText(lessonA.body)).toBeVisible();
    expect(await noOverflow(vp)).toBe(true);
    await vp.screenshot({ path: `test-results/acceptance-step-${lang}-360.png`, fullPage: true });

    // 6. The administrator withdraws lesson B's published revision from the Studio.
    await ad.page.getByRole('navigation', { name: s('shell.nav') }).getByRole('link', { name: s('shell.space.admin') }).click();
    await ad.page.getByLabel(s('admin.lessons.find')).fill(`${baseURL}/learn/lessons/${lessonB.publicId}`);
    await ad.page.getByRole('button', { name: s('admin.lessons.open') }).click();
    const revisionOne = ad.page.getByRole('listitem', { name: s('admin.lesson.revision', { number: 1 }), exact: true });
    await revisionOne.getByRole('button', { name: s('admin.lesson.withdrawThis') }).click();
    await ad.page.getByLabel(s('admin.lesson.reasonLabel')).fill('TEST Erreur dans la leçon B.');
    await revisionOne.getByRole('button', { name: s('admin.lesson.withdrawSubmit') }).click();
    await ad.page.getByRole('dialog', { name: s('admin.lesson.confirmWithdraw', { number: 1 }) }).getByRole('button', { name: s('admin.lesson.withdrawForGood') }).click();
    await expect(ad.page.getByText(s('admin.lesson.withdrawnDone', { number: 1 }))).toBeVisible();

    // The student sees the unavailable step, and navigation stops at it with the notice.
    await steps.getByRole('link', { name: new RegExp(r('step.next')) }).click();
    await expect(vp).toHaveURL(new RegExp(`${stepBURL}$`));
    await expect(vp.getByRole('status')).toContainText(r('step.unavailable'));
    await expect(vp.getByText(lessonB.body)).toHaveCount(0);
    await steps.getByRole('link', { name: new RegExp(r('step.previous')) }).click();
    await expect(vp.getByText(lessonA.body)).toBeVisible();
    await expect(steps.getByRole('link', { name: new RegExp(r('step.next')) })).toContainText(r('step.unavailableShort'));
    // The withdrawn body is not served at the standalone address either.
    const standalone = await vp.request.get(`/learn/lessons/${lessonB.publicId}`);
    expect(standalone.status()).toBe(404);
    expect(await standalone.text()).not.toContain(lessonB.body);

    // A corrected revision of lesson B is written, reviewed and published (lesson workflow through the API).
    const correction = `TEST Corps corrigé de la leçon B ${lang}.`;
    const draft = await t.call('POST', `/api/lessons/${lessonB.lessonId}/drafts`, { content: lessonDoc(lessonB.title, correction), prerequisites: '', chapterId: null, mediaIds: [] });
    await publishLessonRevision(t, rv, pu, draft);

    // The teacher is told and creates an amendment, which goes through review and publication.
    await te.page.goto(`/studio/courses/${courseId}`);
    const notice = te.page.getByRole('region', { name: s('courses.newer.heading') });
    await expect(notice).toContainText(lessonB.title);
    await expect(notice.getByRole('checkbox')).toBeChecked();
    await notice.getByRole('button', { name: s('courses.newer.create') }).click();
    await expect(te.page.getByText(pattern(s('courses.page.amendmentCreated')))).toBeVisible();
    await te.page.getByRole('button', { name: s('courses.editor.submit') }).click();
    await te.page.getByRole('dialog', { name: s('courses.editor.submitTitle', { number: 2 }) }).getByRole('button', { name: s('courses.editor.submitAction'), exact: true }).click();
    await expect(te.page.getByRole('heading', { name: s('courses.status.heading', { number: 2, status: s('shell.status.in_review') }) })).toBeVisible();

    await re.page.goto('/studio/review');
    await re.page.getByRole('listitem').filter({ hasText: courseTitle }).getByRole('link', { name: courseTitle }).click();
    const replaced = re.page.locator('.review-step[data-change="replaced"]');
    await expect(replaced).toHaveCount(1);
    await expect(replaced).toContainText(lessonB.title);
    await expect(re.page.locator('.review-step[data-change="unchanged"]')).toContainText(lessonA.title);
    await re.page.getByRole('button', { name: s('review.course.approveButton') }).click();
    await re.page.getByRole('dialog', { name: s('review.course.approveTitle', { number: 2 }) }).getByRole('button', { name: s('review.decision.approve'), exact: true }).click();
    await expect(re.page.getByText(s('review.course.approved', { title: courseTitle }))).toBeVisible();

    await pb.page.goto('/studio/publish');
    const amended = pb.page.getByRole('list', { name: s('review.coursePublish.list') }).getByRole('listitem').filter({ hasText: courseTitle });
    await expect(amended).toContainText(s('review.replacesPublished'));
    await amended.getByRole('link', { name: courseTitle }).click();
    await pb.page.getByRole('button', { name: s('review.coursePublish.button') }).click();
    await pb.page.getByRole('dialog', { name: s('review.coursePublish.dialogTitle', { title: courseTitle }) }).getByRole('button', { name: s('review.publish'), exact: true }).click();
    await expect(pb.page.getByRole('heading', { name: s('review.coursePublish.done') })).toBeVisible();

    // The step reads again, with the corrected revision, and navigation passes it.
    await steps.getByRole('link', { name: new RegExp(r('step.next')) }).click();
    await expect(vp).toHaveURL(new RegExp(`${stepBURL}$`));
    await expect(vp.getByText(correction)).toBeVisible();
    await expect(vp.getByText(r('step.position', 2, 2))).toBeVisible();
    await expect(vp.getByText(lessonB.body)).toHaveCount(0);
    await expect(vp.getByText(r('step.unavailable'))).toHaveCount(0);
    expect(await noOverflow(vp)).toBe(true);

    for (const x of [ad, te, re, pb]) {
      expect(x.problems).toEqual([]);
      await x.context.close();
    }
    expect(visitorProblems).toEqual([]);
    await visitor.close();
  });
}
