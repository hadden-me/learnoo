import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { OwnedLesson } from '../src/api/client';
import { CatalogueName, catalogueLabel, entryLabel, labelAttrs } from '../src/catalogue';
import { SubjectName, subjectLabel } from '../src/editorial/common';
import { ownedLessonPlace } from '../src/labels';

// Catalogue display names (LRN-014). TEST fixtures, not curriculum data.
const named = { id: 1, code: 'test-math', nameFr: 'TEST Mathématiques', nameAr: 'TEST رياضيات', sortOrder: 0, retiredAt: null };
const arabicOnly = { id: 2, code: 'test-physique', nameFr: null, nameAr: 'TEST فيزياء', sortOrder: 1, retiredAt: null };
const unnamed = { id: 3, code: 'test-chimie', nameFr: null, nameAr: null, sortOrder: 2, retiredAt: null };

describe('catalogue names', () => {
  it('prefers the lesson language, then the other language, then the code', () => {
    expect(entryLabel(named, 'fr')).toEqual({ text: 'TEST Mathématiques', lang: 'fr' });
    expect(entryLabel(named, 'ar')).toEqual({ text: 'TEST رياضيات', lang: 'ar' });
    expect(entryLabel(arabicOnly, 'fr')).toEqual({ text: 'TEST فيزياء', lang: 'ar' });
    expect(catalogueLabel('ch-1', 'TEST Suites', null, 'ar')).toEqual({ text: 'TEST Suites', lang: 'fr' });
    expect(entryLabel(unnamed, 'ar')).toEqual({ text: 'test-chimie', lang: null });
    expect(catalogueLabel('ch-1', '', undefined, 'fr')).toEqual({ text: 'ch-1', lang: null });
  });

  it('marks Arabic names right to left and leaves French names and codes as text', () => {
    const html = (label: ReturnType<typeof entryLabel>) => renderToStaticMarkup(createElement(CatalogueName, { label }));
    expect(html(entryLabel(named, 'ar'))).toBe('<bdi lang="ar" dir="rtl">TEST رياضيات</bdi>');
    expect(html(entryLabel(named, 'fr'))).toBe('TEST Mathématiques');
    expect(html(entryLabel(unnamed, 'fr'))).toBe('test-chimie');
    expect(html({ text: '<b>TEST</b>', lang: 'ar' })).toBe('<bdi lang="ar" dir="rtl">&lt;b&gt;TEST&lt;/b&gt;</bdi>');
    expect(labelAttrs(entryLabel(arabicOnly, 'fr'))).toEqual({ lang: 'ar', dir: 'rtl' });
    expect(labelAttrs(entryLabel(named, 'fr'))).toEqual({});
    // A picker option carries the attributes of its label.
    const option = renderToStaticMarkup(createElement('option', { value: 2, ...labelAttrs(entryLabel(arabicOnly, 'fr')) }, entryLabel(arabicOnly, 'fr').text));
    expect(option).toBe('<option value="2" lang="ar" dir="rtl">TEST فيزياء</option>');
  });

  it('names the subject and chapter of an owned lesson in its language', () => {
    const lesson = {
      subjectCode: 'test-math',
      subjectNameFr: 'TEST Mathématiques',
      subjectNameAr: 'TEST رياضيات',
      language: 'ar',
      latestRevision: { chapterCode: 'ch-1', chapterNameFr: 'TEST Suites', chapterNameAr: null },
    } as unknown as OwnedLesson;
    expect(ownedLessonPlace(lesson)).toEqual({ subject: { text: 'TEST رياضيات', lang: 'ar' }, chapter: { text: 'TEST Suites', lang: 'fr' } });
    const noChapter = { ...lesson, language: 'fr', latestRevision: { chapterCode: null } } as unknown as OwnedLesson;
    expect(ownedLessonPlace(noChapter)).toEqual({ subject: { text: 'TEST Mathématiques', lang: 'fr' }, chapter: null });
  });

  it('names subjects on the editorial pages in French first, with the ID as last resort', () => {
    const subjects = new Map([named, arabicOnly, unnamed].map((s) => [s.id, s]));
    expect(subjectLabel(subjects, 1)).toBe('TEST Mathématiques');
    expect(subjectLabel(subjects, 3)).toBe('test-chimie');
    expect(subjectLabel(subjects, 9)).toBe('matière n° 9');
    expect(renderToStaticMarkup(createElement(SubjectName, { subjects, id: 2 }))).toBe('<bdi lang="ar" dir="rtl">TEST فيزياء</bdi>');
  });
});
