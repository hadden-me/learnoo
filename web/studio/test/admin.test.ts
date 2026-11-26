import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it, vi } from 'vitest';
import { adminApi, currentEditionId, describeAdminError, invitationLink, move, selfRules, userQuery, type EditionSummary } from '../src/api/admin';
import { Api, ApiError } from '../src/api/client';
import { entryChanges } from '../src/editorial/CataloguePage';
import { entryFields } from '../src/editorial/CurriculumPages';
import { describeUserEvent, needsConfirmation } from '../src/editorial/UserPages';
import { editorialPage, routeAccess } from '../src/editorial/spacesUi';
import { isEditorialRoute, parseEditorialRoute, paths } from '../src/editorialRoutes';
import { parseRoute } from '../src/router';
import { NO_SPACES, type Spaces } from '../src/spaces';

// Administration pages (LMS-B10). TEST values only.
type Call = { url: string; method: string; headers: Record<string, string>; body: unknown };

function fakeServer() {
  const calls: Call[] = [];
  const fetch = vi.fn(async (url: string, init: RequestInit) => {
    calls.push({ url, method: init.method ?? 'GET', headers: init.headers as Record<string, string>, body: init.body ? JSON.parse(init.body as string) : undefined });
    const body = url === '/api/auth/session' ? session : { users: [], nextAfter: null, subjects: [], levels: [], section: {}, editions: [] };
    return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
  });
  return { fetch: fetch as unknown as typeof globalThis.fetch, calls };
}

const session = { user: { id: 1, email: 'test-admin@example.test', displayName: 'TEST' }, csrfToken: 'tok-1', expiresAt: '2026-11-25T09:00:00Z' };

describe('administration API', () => {
  it('sends the documented paths and bodies, with the CSRF token on writes only', async () => {
    const server = fakeServer();
    const ad = adminApi(new Api({ fetch: server.fetch }));
    await ad.users();
    await ad.users({ q: '  amel ', permission: 'author', status: 'active', after: 50 });
    await ad.user(5);
    await ad.grant(5, 'author');
    await ad.grant(5, 'reviewer', 3);
    await ad.revoke(5, 'administrator');
    await ad.revoke(5, 'reviewer', 3);
    await ad.disable(5);
    await ad.enable(5);
    await ad.invite('student@example.test', 72);
    await ad.catalogue();
    await ad.setSubjectNames('test-math', { nameFr: 'TEST Maths' });
    await ad.setSubjectOrder('test-math', 10);
    await ad.setChapterNames('test-math', 'ch-1', { nameAr: 'TEST فصل' });
    await ad.setChapterOrder('test-math', 'ch-1', 2);
    await ad.levels();
    await ad.editions(3);
    await ad.edition(7);
    const f = { nameFr: 'TEST Niveau', nameAr: 'TEST مستوى', displayOrder: 1 };
    await ad.createLevel('test-level', f);
    await ad.updateLevel(2, 4, f);
    await ad.retireLevel(2, 5);
    await ad.createSection(2, 'test-sec', f);
    await ad.updateSection(3, 1, f);
    await ad.retireSection(3, 2);
    await ad.createEdition(3, 'test-2026', 'TEST 2026', 'TEST ٢٠٢٦');
    await ad.renameEdition(7, 1, 'A', 'B');
    await ad.addAssignment(7, 2, 1);
    await ad.removeAssignment(7, 11, 3);
    await ad.setChapters(7, 11, 4, [5, 4]);
    await ad.makeCurrent(7, 5, null);
    await ad.retireEdition(7, 6);
    const sent = server.calls.filter((c) => c.url !== '/api/auth/session').map((c) => [c.method, c.url, c.body]);
    const cur = '/api/admin/curriculum';
    expect(sent).toEqual([
      ['GET', '/api/admin/users', undefined],
      ['GET', '/api/admin/users?q=amel&permission=author&status=active&after=50', undefined],
      ['GET', '/api/admin/users/5', undefined],
      ['POST', '/api/admin/users/5/grant', { permission: 'author' }],
      ['POST', '/api/admin/users/5/grant', { permission: 'reviewer', subjectId: 3 }],
      ['POST', '/api/admin/users/5/revoke', { permission: 'administrator' }],
      ['POST', '/api/admin/users/5/revoke', { permission: 'reviewer', subjectId: 3 }],
      ['POST', '/api/admin/users/5/disable', {}],
      ['POST', '/api/admin/users/5/enable', {}],
      ['POST', '/api/admin/invitations', { email: 'student@example.test', ttlHours: 72 }],
      ['GET', '/api/admin/catalogue', undefined],
      ['PUT', '/api/admin/catalogue/subjects/test-math/names', { nameFr: 'TEST Maths' }],
      ['PUT', '/api/admin/catalogue/subjects/test-math/order', { sortOrder: 10 }],
      ['PUT', '/api/admin/catalogue/subjects/test-math/chapters/ch-1/names', { nameAr: 'TEST فصل' }],
      ['PUT', '/api/admin/catalogue/subjects/test-math/chapters/ch-1/order', { sortOrder: 2 }],
      ['GET', '/api/catalogue/levels', undefined],
      ['GET', '/api/catalogue/sections/3/editions', undefined],
      ['GET', '/api/catalogue/editions/7', undefined],
      ['POST', `${cur}/levels`, { code: 'test-level', ...f }],
      ['PUT', `${cur}/levels/2`, { expectedVersion: 4, ...f }],
      ['POST', `${cur}/levels/2/retire`, { expectedVersion: 5 }],
      ['POST', `${cur}/levels/2/sections`, { code: 'test-sec', ...f }],
      ['PUT', `${cur}/sections/3`, { expectedVersion: 1, ...f }],
      ['POST', `${cur}/sections/3/retire`, { expectedVersion: 2 }],
      ['POST', `${cur}/sections/3/editions`, { code: 'test-2026', labelFr: 'TEST 2026', labelAr: 'TEST ٢٠٢٦' }],
      ['PUT', `${cur}/editions/7`, { expectedVersion: 1, labelFr: 'A', labelAr: 'B' }],
      ['POST', `${cur}/editions/7/assignments`, { expectedVersion: 2, subjectId: 1 }],
      ['DELETE', `${cur}/editions/7/assignments/11?expectedVersion=3`, undefined],
      ['PUT', `${cur}/editions/7/assignments/11/chapters`, { expectedVersion: 4, chapterIds: [5, 4] }],
      ['POST', `${cur}/editions/7/make-current`, { expectedVersion: 5, expectedCurrentEditionId: null }],
      ['POST', `${cur}/editions/7/retire`, { expectedVersion: 6 }],
    ]);
    for (const c of server.calls.filter((c) => c.method !== 'GET')) expect(c.headers['X-CSRF-Token']).toBe('tok-1');
    for (const c of server.calls.filter((c) => c.method === 'GET')) expect(c.headers['X-CSRF-Token']).toBeUndefined();
  });

  it('leaves empty filters and a missing lifetime out', async () => {
    expect(userQuery({ q: '   ', permission: '', status: '' })).toBe('');
    expect(userQuery({ permission: 'none' })).toBe('?permission=none');
    const server = fakeServer();
    await adminApi(new Api({ fetch: server.fetch })).invite('student@example.test');
    expect(server.calls.at(-1)!.body).toEqual({ email: 'student@example.test' });
  });

  it('explains the administration refusals in French', () => {
    const msg = (code: string, message = code) => describeAdminError(new ApiError(409, code, message));
    expect(msg('self_change')).toContain('votre propre compte');
    expect(msg('last_administrator')).toContain('dernier administrateur actif');
    expect(msg('account_exists')).toContain('existe déjà');
    expect(msg('version_conflict')).toContain('rechargez la page');
    expect(msg('edition_not_draft')).toContain('brouillon');
    expect(msg('state_conflict')).toContain('Rien n’a été écrit');
    expect(msg('code_exists')).toContain('jamais réutilisés');
    expect(msg('placement_invalid', 'chapter 4 is retired')).toContain('Détail : chapter 4 is retired');
    for (const code of ['invalid_input', 'retired_subject', 'unknown_subject', 'already_retired', 'parent_retired', 'assignment_exists', 'retry'])
      expect(msg(code)).not.toMatch(/Erreur du serveur/);
    // Codes shared with the other spaces keep their explanation.
    expect(msg('forbidden')).toContain('permission nécessaire');
  });
});

describe('administration routes and access', () => {
  it('parses every administration path and builds it back', () => {
    expect(parseRoute('/studio/admin/users')).toEqual({ name: 'adminUsers' });
    expect(parseRoute('/studio/admin/users/12/')).toEqual({ name: 'adminUser', userId: 12 });
    expect(parseRoute('/studio/admin/catalogue')).toEqual({ name: 'adminCatalogue' });
    expect(parseRoute('/studio/admin/curriculum')).toEqual({ name: 'adminCurriculum' });
    expect(parseRoute('/studio/admin/curriculum/sections/3')).toEqual({ name: 'adminSection', sectionId: 3 });
    expect(parseRoute('/studio/admin/curriculum/editions/7')).toEqual({ name: 'adminEdition', editionId: 7 });
    expect(parseRoute('/studio/admin/users/0')).toEqual({ name: 'notFound' });
    expect(parseRoute('/studio/admin/users/abc')).toEqual({ name: 'notFound' });
    for (const p of [paths.adminUsers, paths.adminUser(4), paths.adminCatalogue, paths.adminCurriculum, paths.adminSection(3), paths.adminEdition(7)]) {
      const route = parseEditorialRoute(p);
      expect(route && isEditorialRoute(route)).toBe(true);
      expect(p.split('/').at(-1)).not.toContain('.');
    }
  });

  it('opens administration pages only to administrators', () => {
    const others: Spaces[] = [NO_SPACES, { author: true, reviewer: true, publisher: true, administrator: false }];
    const routes = ['/studio/admin/users', '/studio/admin/users/4', '/studio/admin/catalogue', '/studio/admin/curriculum', '/studio/admin/curriculum/sections/3', '/studio/admin/curriculum/editions/7'].map(
      (p) => parseEditorialRoute(p)!,
    );
    for (const route of routes) {
      expect(routeAccess(route, { ...NO_SPACES, administrator: true })).toEqual({ space: 'Administration', allowed: true });
      for (const s of others) {
        expect(routeAccess(route, s).allowed).toBe(false);
        expect(renderToStaticMarkup(editorialPage(route, s) as React.ReactElement)).toContain('Accès refusé');
      }
    }
  });
});

describe('administration helpers', () => {
  it('moves chapters within bounds', () => {
    expect(move([1, 2, 3], 0, 1)).toEqual([2, 1, 3]);
    expect(move([1, 2, 3], 2, -1)).toEqual([1, 3, 2]);
    expect(move([1, 2, 3], 0, -1)).toEqual([1, 2, 3]);
    expect(move([1, 2, 3], 2, 1)).toEqual([1, 2, 3]);
  });

  it('offers no grant or disable on the administrator’s own account, but keeps revocation', () => {
    expect(selfRules(true)).toEqual({ canGrant: false, canDisable: false, canRevoke: true });
    expect(selfRules(false)).toEqual({ canGrant: true, canDisable: true, canRevoke: true });
  });

  it('confirms revocations, the administrator grant and status changes', () => {
    expect(needsConfirmation({ kind: 'grant', permission: 'author' })).toBe(false);
    expect(needsConfirmation({ kind: 'grant', permission: 'administrator' })).toBe(true);
    expect(needsConfirmation({ kind: 'revoke', permission: 'author' })).toBe(true);
    expect(needsConfirmation({ kind: 'revokeReviewer', subjectId: 1, label: 'x' })).toBe(true);
    expect(needsConfirmation({ kind: 'disable' })).toBe(true);
    expect(needsConfirmation({ kind: 'enable' })).toBe(true);
  });

  it('builds the invitation link from this origin', () => {
    expect(invitationLink('https://learnoo.example/', '/invite/abc')).toBe('https://learnoo.example/invite/abc');
  });

  it('describes history events with the user actor or the operator label', () => {
    const base = { kind: 'permission', action: 'grant', permission: 'reviewer', subjectId: 1, subjectCode: 'test-math', occurredAt: '2026-11-26T10:00:00Z' } as const;
    expect(describeUserEvent({ ...base, actorId: 2, actorName: 'TEST Admin', operatorLabel: null })).toBe('Attribution : Relecteur (test-math), par TEST Admin');
    expect(describeUserEvent({ ...base, kind: 'status', action: 'disable', permission: null, subjectId: null, subjectCode: null, actorId: null, actorName: null, operatorLabel: 'op@host' })).toBe(
      'Désactivation, par opérateur op@host',
    );
  });

  it('finds the section’s current edition', () => {
    const e = (id: number, state: EditionSummary['state']) => ({ id, state }) as EditionSummary;
    expect(currentEditionId([e(1, 'superseded'), e(2, 'current'), e(3, 'draft')])).toBe(2);
    expect(currentEditionId([e(3, 'draft')])).toBeNull();
  });

  it('validates catalogue and curriculum forms before sending', () => {
    const entry = { nameFr: 'TEST A', nameAr: null, sortOrder: 0 };
    expect(entryChanges(entry, 'TEST A', '', '0')).toEqual({ names: null, order: null });
    expect(entryChanges(entry, ' TEST B ', 'TEST ب', '5')).toEqual({ names: { nameFr: 'TEST B', nameAr: 'TEST ب' }, order: 5 });
    expect(entryChanges(entry, '', '', '0')).toHaveProperty('error');
    expect(entryChanges(entry, 'TEST A', '', '10000')).toHaveProperty('error');
    expect(entryFields(' A ', 'B', '3')).toEqual({ nameFr: 'A', nameAr: 'B', displayOrder: 3 });
    expect(entryFields('A', '', '3')).toHaveProperty('error');
    expect(entryFields('A', 'B', '1.5')).toHaveProperty('error');
  });
});
