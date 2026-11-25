import { beforeEach, describe, expect, it } from 'vitest';
import { RECOVERY_PREFIX, differs, findRecovery, recoveryKey, removeRecovery, writeRecovery, type RecoveryData } from '../src/recovery';
import { parseRoute, routePath } from '../src/router';
import { loginURL } from '../src/App';

const data = (title: string): RecoveryData => ({ doc: { type: 'doc', content: [] }, title, objective: '', prerequisites: '', chapterId: null, mediaIds: [] });
const at = (iso: string) => () => new Date(iso);

describe('unsaved-edit recovery buffer', () => {
  beforeEach(() => localStorage.clear());

  it('keys copies by user, revision, base version and editor instance', () => {
    expect(recoveryKey({ userId: 7, revisionId: 12, baseVersion: 3, instance: 'a' })).toBe(`${RECOVERY_PREFIX}u7:r12:v3:a`);
    expect(recoveryKey({ userId: 7, revisionId: null, baseVersion: null, instance: 'a' })).toBe(`${RECOVERY_PREFIX}u7:new:a`);
    expect(recoveryKey({ userId: 7, revisionId: null, baseVersion: null, lessonId: 4, instance: 'a' })).toBe(`${RECOVERY_PREFIX}u7:lesson-4:new:a`);
  });

  it('finds a revision’s copies across versions and tabs, newest first, never another user’s', () => {
    writeRecovery(localStorage, { userId: 7, revisionId: 12, baseVersion: 3, instance: 'a' }, data('v3'), 'conflict', undefined, at('2026-11-25T10:00:00Z'));
    writeRecovery(localStorage, { userId: 7, revisionId: 12, baseVersion: 4, instance: 'b' }, data('v4'), 'unsaved', undefined, at('2026-11-25T11:00:00Z'));
    writeRecovery(localStorage, { userId: 7, revisionId: 13, baseVersion: 1, instance: 'a' }, data('other revision'));
    writeRecovery(localStorage, { userId: 8, revisionId: 12, baseVersion: 3, instance: 'c' }, data('other user'));
    const found = findRecovery(localStorage, 7, 12);
    expect(found.map((e) => e.data.title)).toEqual(['v4', 'v3']);
    expect(found[1].reason).toBe('conflict');
    // The caller's own live copy is excluded.
    expect(findRecovery(localStorage, 7, 12, null, found[0].key).map((e) => e.data.title)).toEqual(['v3']);
  });

  it('keeps new-lesson copies apart from drafts of existing lessons', () => {
    const params = { subjectId: 1, subjectCode: 'maths', language: 'fr' as const, translationOf: null };
    writeRecovery(localStorage, { userId: 7, revisionId: null, baseVersion: null, instance: 'a' }, data('new'), 'unsaved', params);
    writeRecovery(localStorage, { userId: 7, revisionId: null, baseVersion: null, lessonId: 4, instance: 'a' }, data('lesson 4'));
    expect(findRecovery(localStorage, 7, null).map((e) => [e.data.title, e.newLesson?.subjectCode])).toEqual([['new', 'maths']]);
    expect(findRecovery(localStorage, 7, null, 4).map((e) => e.data.title)).toEqual(['lesson 4']);
  });

  it('a second tab never overwrites the first tab’s copy', () => {
    writeRecovery(localStorage, { userId: 7, revisionId: 12, baseVersion: 3, instance: 'a' }, data('tab a'));
    writeRecovery(localStorage, { userId: 7, revisionId: 12, baseVersion: 3, instance: 'b' }, data('tab b'));
    expect(findRecovery(localStorage, 7, 12).map((e) => e.data.title).sort()).toEqual(['tab a', 'tab b']);
  });

  it('ignores tampered or foreign entries instead of throwing, and removes only its own keys', () => {
    localStorage.setItem(`${RECOVERY_PREFIX}u7:r12:v1:x`, '{not json');
    localStorage.setItem(`${RECOVERY_PREFIX}u7:r12:v2:y`, JSON.stringify({ key: 'elsewhere', userId: 7, instance: 'y', data: data('x') }));
    localStorage.setItem('unrelated', 'keep');
    expect(findRecovery(localStorage, 7, 12)).toEqual([]);
    removeRecovery(localStorage, 'unrelated');
    expect(localStorage.getItem('unrelated')).toBe('keep');
  });

  it('never breaks editing when storage is unavailable', () => {
    const broken = {
      get length(): number {
        throw new Error('denied');
      },
      setItem() {
        throw new Error('quota');
      },
    } as unknown as Storage;
    expect(writeRecovery(broken, { userId: 7, revisionId: 1, baseVersion: 1, instance: 'a' }, data('x'))).toBe(false);
    expect(findRecovery(broken, 7, 1)).toEqual([]);
  });

  it('compares edits, including attachments and classification', () => {
    expect(differs(data('a'), data('a'))).toBe(false);
    expect(differs(data('a'), { ...data('a'), mediaIds: [5] })).toBe(true);
    expect(differs(data('a'), { ...data('a'), chapterId: 2 })).toBe(true);
  });
});

describe('routes', () => {
  it('parses Studio paths, none with a dot in the last segment', () => {
    expect(parseRoute('/studio')).toEqual({ name: 'lessons' });
    expect(parseRoute('/studio/')).toEqual({ name: 'lessons' });
    expect(parseRoute('/studio/lessons/new')).toEqual({ name: 'new' });
    expect(parseRoute('/studio/lessons/42/edit')).toEqual({ name: 'edit', lessonId: 42 });
    expect(parseRoute('/studio/lessons/0/edit')).toEqual({ name: 'notFound' });
    expect(parseRoute('/studio/lessons/abc/edit')).toEqual({ name: 'notFound' });
    for (const route of [{ name: 'lessons' }, { name: 'new' }, { name: 'edit', lessonId: 9 }] as const) {
      const path = routePath(route);
      expect(path.split('/').at(-1)).not.toContain('.');
      expect(parseRoute(path)).toEqual(route);
    }
  });

  it('sends only plain /studio paths as the sign-in return address', () => {
    expect(loginURL('/studio/lessons/42/edit')).toBe('/login?next=%2Fstudio%2Flessons%2F42%2Fedit');
    expect(loginURL('/studio/')).toBe('/login?next=%2Fstudio');
    expect(loginURL('/studio/../learn')).toBe('/login?next=%2Fstudio');
  });
});
