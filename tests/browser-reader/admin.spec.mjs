import { test, expect } from '@playwright/test';

// The Studio's administration pages (LMS-B10) against the Go application and
// the fixture server's PostgreSQL schema (internal/server
// TestBrowserReaderServer). Simulated TEST accounts only, with the test
// password from server_test.go: test-admin (administrator only),
// test-author (author), test-onboard (no permission: changed only here),
// test-suspend (author: changed only here). Curriculum entries created here
// are TEST codes in this schema; no catalogue name is changed, since other
// tests read them. Desktop Chromium; the 360px run emulates a phone
// viewport, not a physical phone.
const password = 'simulated-password-1';
const account = (name) => ({ email: `${name}@example.test`, password });
const admin = account('test-admin');
const author = account('test-author');
const onboard = account('test-onboard');
const suspend = account('test-suspend');

const desktop = { width: 1280, height: 900 };
const phone = { width: 360, height: 780 };

/** Fails the test on any CSP violation or request leaving the application's origin. */
function watch(page, baseURL) {
  const problems = [];
  page.on('console', (m) => {
    if (/Content Security Policy|Refused to/i.test(m.text())) problems.push(m.text());
  });
  page.on('request', (r) => {
    if (new URL(r.url()).origin !== new URL(baseURL).origin) problems.push(`external request ${r.url()}`);
  });
  return problems;
}

async function signIn(page, user) {
  await expect(page).toHaveURL(/\/login\?next=/);
  await page.getByLabel('Adresse e-mail').fill(user.email);
  await page.getByLabel('Mot de passe').fill(user.password);
  await page.getByRole('button', { name: 'Se connecter' }).click();
}

/** A fresh browser context signed in as `user`, opened at `path`. */
async function signedIn(browser, baseURL, user, { path = '/studio', viewport = desktop } = {}) {
  const context = await browser.newContext({ baseURL, viewport });
  const page = await context.newPage();
  const problems = watch(page, baseURL);
  await page.goto(path);
  await signIn(page, user);
  await expect(page).not.toHaveURL(/\/login/);
  return { context, page, problems };
}

const nav = (page) => page.getByRole('navigation', { name: 'Studio' });
const adminNav = (page) => page.getByRole('navigation', { name: 'Administration' });
const noOverflow = (page) => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth);
const csrf = async (page) => (await (await page.request.get('/api/auth/session')).json()).csrfToken;

/** Finds a user through the search form and opens their page. */
async function openUser(page, query, name) {
  await adminNav(page).getByRole('link', { name: 'Utilisateurs' }).click();
  await expect(page.getByRole('heading', { name: 'Utilisateurs', level: 1 })).toBeVisible();
  await page.getByLabel('Rechercher (e-mail ou nom)').fill(query);
  await page.getByRole('button', { name: 'Rechercher', exact: true }).click();
  const list = page.getByRole('list', { name: 'Utilisateurs' });
  await expect(list.getByRole('link')).toHaveText([name]);
  await list.getByRole('link', { name }).click();
  await expect(page.getByRole('heading', { name, level: 1 })).toBeVisible();
}

test('an administrator grants reviewer on a subject, and the reviewer then sees the review queue', async ({ browser, baseURL }) => {
  // Before the grant the account has no Studio access at all.
  const before = await browser.newContext({ baseURL });
  const bp = await before.newPage();
  await bp.goto('/studio');
  await signIn(bp, onboard);
  await expect(bp.getByRole('heading', { name: 'Accès réservé' })).toBeVisible();
  await before.close();

  const a = await signedIn(browser, baseURL, admin);
  await nav(a.page).getByRole('link', { name: 'Administration' }).click();
  await openUser(a.page, 'onboard', 'TEST Enseignant à intégrer');
  await expect(a.page.getByText('Aucune matière de relecture.')).toBeVisible();
  // Administrators are not reviewers unless a subject is granted.
  await expect(a.page.getByText('La permission administrateur ne donne pas le droit de relire')).toBeVisible();
  await a.page.getByLabel('Ajouter une matière de relecture').selectOption({ label: 'test-physique' });
  await a.page.getByRole('button', { name: 'Attribuer la relecture' }).click();
  await expect(a.page.getByText('Matière de relecture attribuée.')).toBeVisible();
  await expect(a.page.getByRole('list', { name: 'Matières de relecture' })).toContainText('test-physique');
  // Audited with the administrator as actor.
  await expect(a.page.getByText('par TEST Admin Lina').first()).toBeVisible();

  // The administrator's own page offers no self-grant and no self-disable.
  await openUser(a.page, 'test-admin@', 'TEST Admin Lina');
  await expect(a.page.getByText('C’est votre propre compte')).toBeVisible();
  await expect(a.page.getByRole('button', { name: /^Attribuer/ })).toHaveCount(0);
  await expect(a.page.getByRole('button', { name: 'Désactiver le compte' })).toHaveCount(0);
  // …and the server refuses them anyway.
  const self = await a.page.request.post(`/api/admin/users/${/\/users\/(\d+)$/.exec(a.page.url())[1]}/grant`, {
    data: { permission: 'author' },
    headers: { 'X-CSRF-Token': await csrf(a.page) },
  });
  expect(self.status()).toBe(403);
  expect((await self.json()).error.code).toBe('self_change');

  // The new reviewer sees the review space, scoped to the granted subject.
  const r = await signedIn(browser, baseURL, onboard);
  await expect(nav(r.page).getByRole('link')).toHaveText(['Relecture', 'Signalements']);
  await nav(r.page).getByRole('link', { name: 'Relecture' }).click();
  await expect(r.page.getByRole('heading', { name: 'Relecture', level: 1 })).toBeVisible();
  await expect(r.page.getByText('Aucune révision n’attend de relecture dans vos matières.')).toBeVisible();

  for (const who of [a, r]) {
    expect(who.problems).toEqual([]);
    await who.context.close();
  }
});

test('an administrator disables an account, which is signed out at once, then enables it again', async ({ browser, baseURL }) => {
  const s = await signedIn(browser, baseURL, suspend);
  await expect(nav(s.page).getByRole('link', { name: 'Mes leçons' })).toBeVisible();

  const a = await signedIn(browser, baseURL, admin, { path: '/studio/admin/users' });
  await openUser(a.page, 'test-suspend', 'TEST Compte à suspendre');
  await a.page.getByRole('button', { name: 'Désactiver le compte' }).click();
  const dialog = a.page.getByRole('dialog', { name: 'Désactiver le compte de TEST Compte à suspendre ?' });
  await dialog.getByRole('button', { name: 'Désactiver', exact: true }).click();
  await expect(a.page.getByText('Compte désactivé : ses sessions sont fermées.')).toBeVisible();
  await expect(a.page.getByText('Ce compte est désactivé')).toBeVisible();

  // The disabled account's session is gone and it cannot sign in.
  expect((await s.page.request.get('/api/lessons/mine')).status()).toBe(401);
  await s.page.goto('/studio');
  await signIn(s.page, suspend);
  await expect(s.page).toHaveURL(/\/login/);

  await a.page.getByRole('button', { name: 'Réactiver le compte' }).click();
  await a.page.getByRole('dialog', { name: 'Réactiver le compte de TEST Compte à suspendre ?' }).getByRole('button', { name: 'Réactiver', exact: true }).click();
  await expect(a.page.getByText('Compte réactivé.')).toBeVisible();
  await expect(a.page.getByText('Ce compte est actif.')).toBeVisible();
  await expect(a.page.getByText('Désactivation, par TEST Admin Lina')).toBeVisible();

  // Its grants apply again.
  const again = await signedIn(browser, baseURL, suspend);
  await expect(nav(again.page).getByRole('link', { name: 'Mes leçons' })).toBeVisible();

  for (const who of [a, again]) {
    expect(who.problems).toEqual([]);
    await who.context.close();
  }
  await s.context.close();
});

test('an administrator builds a curriculum edition and makes it current, at 360px', async ({ browser, baseURL }) => {
  const a = await signedIn(browser, baseURL, admin, { path: '/studio/admin/curriculum', viewport: phone });
  await expect(a.page.getByRole('heading', { name: 'Programmes', level: 1 })).toBeVisible();
  expect(await noOverflow(a.page)).toBe(true);

  await a.page.getByText('Nouveau niveau', { exact: true }).click();
  const level = a.page.getByRole('form', { name: 'Nouveau niveau' });
  await level.getByLabel('Code').fill('test-bac');
  await level.getByLabel('Nom français').fill('TEST Baccalauréat');
  await level.getByLabel('Nom arabe').fill('TEST بكالوريا');
  await level.getByRole('button', { name: 'Créer le niveau' }).click();
  await expect(a.page.getByText('Niveau test-bac créé.')).toBeVisible();

  await a.page.getByText('Nouvelle section dans test-bac', { exact: true }).click();
  const section = a.page.getByRole('form', { name: 'Nouvelle section dans test-bac' });
  await section.getByLabel('Code').fill('test-sciences');
  await section.getByLabel('Nom français').fill('TEST Sciences');
  await section.getByLabel('Nom arabe').fill('TEST علوم');
  await section.getByRole('button', { name: 'Créer la section' }).click();
  await a.page.getByRole('link', { name: 'Éditions de la section test-sciences' }).click();
  await expect(a.page.getByRole('heading', { name: 'Éditions de la section TEST Sciences', level: 1 })).toBeVisible();
  await expect(a.page.getByText('Aucune édition pour cette section.')).toBeVisible();

  const edition = a.page.getByRole('form', { name: 'Nouvelle édition' });
  await edition.getByLabel('Code').fill('test-2026');
  await edition.getByLabel('Libellé français').fill('TEST Programme 2026');
  await edition.getByLabel('Libellé arabe').fill('TEST برنامج');
  await edition.getByRole('button', { name: 'Créer l’édition' }).click();
  await expect(a.page.getByRole('heading', { name: 'Édition TEST Programme 2026', level: 1 })).toBeVisible();

  await a.page.getByLabel('Matière à ajouter').selectOption({ label: 'TEST Mathématiques (test-math)' });
  await a.page.getByRole('button', { name: 'Ajouter la matière' }).click();
  await expect(a.page.getByText('Matière ajoutée à l’édition.')).toBeVisible();
  await a.page.getByLabel('Chapitre à ajouter (test-math)').selectOption({ label: 'TEST Nombres complexes (test-chapter)' });
  await a.page.getByRole('button', { name: 'Ajouter le chapitre' }).click();
  await a.page.getByRole('button', { name: 'Enregistrer la liste des chapitres' }).click();
  await expect(a.page.getByText('Liste des chapitres de test-math enregistrée.')).toBeVisible();
  await expect(a.page.getByRole('list', { name: 'Chapitres de test-math' })).toContainText('TEST Nombres complexes');
  expect(await noOverflow(a.page)).toBe(true);

  await a.page.getByRole('button', { name: 'Mettre en vigueur' }).click();
  const dialog = a.page.getByRole('dialog', { name: 'Mettre cette édition en vigueur ?' });
  await expect(dialog).toContainText('ne peut pas être annulée');
  await dialog.getByRole('button', { name: 'Mettre en vigueur' }).click();
  await expect(a.page.getByText('Édition mise en vigueur.')).toBeVisible();
  await expect(a.page.getByText('Sélectionnable', { exact: true })).toBeVisible();
  // A current edition's assignments are frozen.
  await expect(a.page.getByRole('button', { name: 'Ajouter la matière' })).toHaveCount(0);
  await expect(a.page.getByRole('button', { name: 'Retirer l’édition' })).toBeVisible();
  expect(await noOverflow(a.page)).toBe(true);

  expect(a.problems).toEqual([]);
  await a.context.close();
});

test('an administrator invites a student, who becomes a teacher once granted author', async ({ browser, baseURL }) => {
  const a = await signedIn(browser, baseURL, admin, { path: '/studio/admin/users' });
  await a.page.getByLabel('Adresse e-mail de l’élève').fill('test-new-teacher@example.test');
  await a.page.getByRole('button', { name: 'Créer l’invitation' }).click();
  const link = await a.page.getByLabel('Lien d’invitation').inputValue();
  expect(link).toMatch(new RegExp(`^${baseURL}/invite/[A-Za-z0-9_-]{43}$`));

  const t = await browser.newContext({ baseURL });
  const tp = await t.newPage();
  await tp.goto(link);
  await tp.getByLabel('Prénom ou pseudonyme').fill('TEST Nouvelle enseignante');
  await tp.getByLabel('Mot de passe', { exact: true }).fill(password);
  await tp.getByLabel('Confirmer le mot de passe').fill(password);
  await tp.getByRole('button', { name: 'Activer mon compte' }).click();
  await expect(tp).toHaveURL(/\/learn$/);

  await a.page.getByLabel('Permission').selectOption({ label: 'Élèves (aucune permission)' });
  await openUser(a.page, 'new-teacher', 'TEST Nouvelle enseignante');
  await a.page.getByRole('button', { name: 'Attribuer Auteur' }).click();
  await expect(a.page.getByText('Permission Auteur attribuée.')).toBeVisible();

  await tp.goto('/studio');
  await expect(nav(tp).getByRole('link', { name: 'Mes leçons' })).toBeVisible();
  expect(a.problems).toEqual([]);
  await a.context.close();
  await t.close();
});

test('accounts without administrator see no administration pages, and the server refuses them', async ({ browser, baseURL }) => {
  const u = await signedIn(browser, baseURL, author);
  await expect(nav(u.page).getByRole('link', { name: 'Administration' })).toHaveCount(0);
  for (const path of ['/studio/admin/users', '/studio/admin/users/1', '/studio/admin/catalogue', '/studio/admin/curriculum', '/studio/admin/curriculum/editions/1']) {
    await u.page.goto(path);
    await expect(u.page.getByRole('heading', { name: 'Accès refusé' })).toBeVisible();
    await expect(u.page.getByText('« Administration »')).toBeVisible();
  }
  const token = await csrf(u.page);
  expect((await u.page.request.get('/api/admin/users')).status()).toBe(403);
  for (const [path, data] of [
    ['/api/admin/users/1/grant', { permission: 'administrator' }],
    ['/api/admin/users/1/disable', {}],
    ['/api/admin/invitations', { email: 'x@example.test' }],
  ]) {
    const res = await u.page.request.post(path, { data, headers: { 'X-CSRF-Token': token } });
    expect(res.status()).toBe(403);
    expect((await res.json()).error.code).toBe('forbidden');
  }
  expect(u.problems).toEqual([]);
  await u.context.close();
});
