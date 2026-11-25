// Uploaded images embedded in the text (issue #77): "media:<id>" references
// with required alt text and an optional caption.
import { describe, expect, it } from 'vitest';
import { buildFields } from '../src/drafts';
import { embeddedMediaIds, mediaId, parseLesson, serializeLesson } from '../src/schema/lesson';
import { describePaste, sanitizePastedHTML } from '../src/schema/paste';
import { createEditor, exportFrom, loadInto, roundTrip } from './helpers';

const meta = { title: 'Images — test technique', language: 'fr' as const, objective: '' };
const doc = (...blocks: unknown[]) => JSON.stringify({ schemaVersion: 1, title: meta.title, language: meta.language, status: 'unreviewed', objective: meta.objective, blocks }, null, 2);
const para = (text: string) => ({ type: 'paragraph', children: [{ type: 'text', text }] });

describe('media references', () => {
  it('reads only well-formed IDs', () => {
    expect(mediaId('media:12')).toBe(12);
    // The longest valid ID is read exactly (below 2^53).
    expect(mediaId('media:999999999999999')).toBe(999999999999999);
    for (const src of ['media:0', 'media:012', 'media:-1', 'media:', 'media:1 ', '/api/media/1', 'media:1000000000000000', 'media:9007199254740993', null, undefined]) expect(mediaId(src)).toBeNull();
  });

  it('lists distinct embedded IDs, nested ones included', () => {
    const lesson = parseLesson(doc({ type: 'image', src: 'media:9', alt: 'a' }, { type: 'callout', title: 'C', children: [{ type: 'image', src: 'media:3', alt: 'b' }, { type: 'image', src: 'media:9', alt: 'c' }] }, { type: 'image', src: '/static/complex-plane.svg', alt: 'd' }));
    expect(embeddedMediaIds(lesson.blocks)).toEqual([3, 9]);
  });

  it('round-trips an uploaded image with its caption byte for byte', () => {
    const source = doc(para('Avant'), { type: 'image', src: 'media:5', alt: 'Un schéma', caption: 'Figure 1' }, { type: 'image', src: '/static/complex-plane.svg', alt: 'Plan' });
    const { result } = roundTrip(source);
    expect(result.errors).toEqual([]);
    expect(serializeLesson(result.document)).toBe(source);
  });

  it('trims alt text and caption, drops an empty caption and refuses an empty alt', () => {
    const editor = createEditor();
    editor.commands.setContent({ type: 'doc', content: [{ type: 'image', attrs: { src: 'media:5', alt: '  Schéma ', caption: '   ' } }] });
    expect(exportFrom(editor, meta).document.blocks).toEqual([{ type: 'image', src: 'media:5', alt: 'Schéma' }]);
    editor.commands.setContent({ type: 'doc', content: [{ type: 'image', attrs: { src: 'media:5', alt: '   ', caption: 'Légende' } }] });
    expect(exportFrom(editor, meta).errors.join(' ')).toMatch(/texte alternatif/);
  });

  it('shows uploaded images through the author media route and parses its own markup back', () => {
    const editor = createEditor();
    loadInto(editor, parseLesson(doc({ type: 'image', src: 'media:5', alt: 'Schéma', caption: 'Légende' })));
    const html = editor.getHTML();
    expect(html).toContain('src="/api/media/5"');
    expect(html).toContain('data-src="media:5"');
    const copy = createEditor();
    copy.commands.setContent(html);
    expect(exportFrom(copy, meta).document.blocks).toEqual([{ type: 'image', src: 'media:5', alt: 'Schéma', caption: 'Légende' }]);
  });

  it('keeps the attached IDs in sync with the text when saving', () => {
    const editor = createEditor();
    loadInto(editor, parseLesson(doc(para('x'), { type: 'image', src: 'media:8', alt: 'a' }, { type: 'image', src: 'media:2', alt: 'b' })));
    const built = buildFields({ doc: editor.getJSON(), ...meta, prerequisites: '', chapterId: null, mediaIds: [4, 8] });
    expect(built.errors).toEqual([]);
    // Placed images are attached; attached images no longer in the text stay attached.
    expect(built.fields?.mediaIds).toEqual([4, 8, 2]);
  });
});

describe('pasting images', () => {
  it('keeps uploaded-image figures copied inside the Studio', () => {
    const editor = createEditor();
    loadInto(editor, parseLesson(doc({ type: 'image', src: 'media:5', alt: 'Schéma' })));
    const { html, report } = sanitizePastedHTML(editor.getHTML());
    expect(report.removed).toEqual([]);
    const target = createEditor();
    target.commands.setContent(html);
    expect(exportFrom(target, meta).document.blocks).toEqual([{ type: 'image', src: 'media:5', alt: 'Schéma' }]);
  });

  it('drops reader and API image URLs instead of turning them into references', () => {
    for (const html of [
      '<figure class="lesson-media"><img src="/learn/lessons/00000000-0000-4000-8000-000000000000/media/5" alt="x"></figure><p>t</p>',
      '<img src="/api/media/5" alt="x"><p>t</p>',
      '<figure data-type="image" data-src="/api/media/5"><img src="/api/media/5" alt="x"></figure><p>t</p>',
      '<figure data-type="image" data-src="media:05"><img src="x" alt="x"></figure><p>t</p>',
    ]) {
      const { html: clean, report } = sanitizePastedHTML(html);
      expect(describePaste(report)).toMatch(/image externe/);
      const editor = createEditor();
      editor.commands.setContent(clean);
      expect(exportFrom(editor, meta).document.blocks.some((b) => b.type === 'image')).toBe(false);
    }
  });
});
