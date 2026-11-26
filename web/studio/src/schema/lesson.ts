// Restricted lesson JSON (schemaVersion 1). Mirrors internal/rendering/render.go;
// docs/fixtures/README.md is the contract. The Go renderer stays authoritative:
// this module only lets the Studio refuse invalid documents before sending them.
import { t } from '../i18n';

export type Lang = 'fr' | 'ar';
export type Dir = 'ltr' | 'rtl' | 'auto';

export interface LessonNode {
  type: string;
  text?: string;
  title?: string;
  lang?: Lang;
  dir?: Dir;
  src?: string;
  alt?: string;
  caption?: string;
  videoId?: string;
  children?: LessonNode[];
}

export interface LessonDocument {
  schemaVersion: 1;
  title: string;
  language: Lang;
  status: 'unreviewed';
  objective: string;
  blocks: LessonNode[];
}

export const MAX_DOCUMENT_BYTES = 128 << 10;
export const MAX_NODES = 500;
export const MAX_DEPTH = 12;
export const ALLOWED_IMAGES = ['/static/complex-plane.svg'] as const;
/**
 * An uploaded asset embedded by its immutable ID (rendering.MediaID): positive, no sign or leading zero,
 * at most 15 digits, so every valid ID is below 2^53 and Number() reads it exactly.
 */
export const MEDIA_REF = /^media:[1-9][0-9]{0,14}$/;
/** White space only: the union of Go's unicode.IsSpace and String.prototype.trim (rendering.blankText). */
export const BLANK = /^[\t\n\v\f\r \u0085\u00A0\u1680\u2000-\u200A\u2028\u2029\u202F\u205F\u3000\uFEFF]*$/;
export const MEDIA_PREFIX = 'media:';

/** The uploaded asset ID an image src refers to, or null. */
export function mediaId(src: string | undefined | null): number | null {
  return src && MEDIA_REF.test(src) ? Number(src.slice(MEDIA_PREFIX.length)) : null;
}

/** Distinct uploaded asset IDs embedded in the blocks, sorted (rendering.MediaIDs). */
export function embeddedMediaIds(blocks: LessonNode[]): number[] {
  const ids = new Set<number>();
  const walk = (nodes: LessonNode[]) => {
    for (const n of nodes) {
      const id = n.type === 'image' ? mediaId(n.src) : null;
      if (id !== null) ids.add(id);
      walk(n.children ?? []);
    }
  };
  walk(blocks);
  return [...ids].sort((a, b) => a - b);
}

export const isAllowedImageSrc = (src: string) => (ALLOWED_IMAGES as readonly string[]).includes(src) || MEDIA_REF.test(src);
export const YOUTUBE_ID = /^[A-Za-z0-9_-]{11}$/;

const DOCUMENT_KEYS = ['schemaVersion', 'title', 'language', 'status', 'objective', 'blocks'];
// Go struct field order; also the order used when serialising nodes.
export const NODE_KEYS = ['type', 'text', 'title', 'lang', 'dir', 'src', 'alt', 'caption', 'videoId', 'children'] as const;
const STRING_NODE_KEYS = new Set(['type', 'text', 'title', 'lang', 'dir', 'src', 'alt', 'caption', 'videoId']);

const INLINE = new Set(['text', 'math', 'strong']);
const INLINE_PARENTS = new Set(['paragraph', 'heading', 'strong']);
const LEAVES = new Set(['text', 'math', 'displayMath']);
const CONTAINERS = new Set(['paragraph', 'heading', 'strong', 'list', 'item', 'callout', 'exercise', 'solution']);

export class LessonError extends Error {}

const utf8 = new TextEncoder();
export const byteLength = (value: string) => utf8.encode(value).length;

/**
 * Strict JSON decoding, matching internal/strictjson on the Go side: keys must
 * be exact (JSON.parse is case-sensitive), unique in every object (JSON.parse
 * would keep the last one), and nothing may follow the single value. null is
 * never a valid field value (the type checks below refuse it), and every
 * number must be spelled as a plain integer that fits Go's int, because
 * JSON.parse reads 1.0 and 1e0 as 1 where Go refuses them.
 */
export function parseLesson(source: string): LessonDocument {
  if (byteLength(source) > MAX_DOCUMENT_BYTES) throw new LessonError(t('lessons.check.tooLarge'));
  let raw: unknown;
  try {
    raw = JSON.parse(source);
  } catch {
    throw new LessonError(t('lessons.check.invalidJson'));
  }
  const duplicate = findDuplicateKey(source);
  if (duplicate !== null) throw new LessonError(t('lessons.check.duplicateKey', { key: duplicate }));
  const number = findNonIntegerNumber(source);
  if (number !== null) throw new LessonError(t('lessons.check.invalidNumber', { number }));
  if (!isRecord(raw)) throw new LessonError(t('lessons.check.notObject'));
  for (const key of Object.keys(raw)) {
    if (!DOCUMENT_KEYS.includes(key)) throw new LessonError(t('lessons.check.unknownDocumentField', { key }));
  }
  for (const key of ['title', 'language', 'status', 'objective']) {
    if (key in raw && typeof raw[key] !== 'string') throw new LessonError(t('lessons.check.fieldText', { key }));
  }
  if ('schemaVersion' in raw && typeof raw.schemaVersion !== 'number') throw new LessonError(t('lessons.check.mustBeNumber', { field: 'schemaVersion' }));
  if ('blocks' in raw && !Array.isArray(raw.blocks)) throw new LessonError(t('lessons.check.mustBeList', { field: 'blocks' }));
  const doc = {
    schemaVersion: raw.schemaVersion ?? 0,
    title: raw.title ?? '',
    language: raw.language ?? '',
    status: raw.status ?? '',
    objective: raw.objective ?? '',
    blocks: ((raw.blocks as unknown[]) ?? []).map((node, i) => parseNode(node, `blocks[${i}]`)),
  } as LessonDocument;
  const errors = validateLesson(doc);
  if (errors.length > 0) throw new LessonError(errors[0]);
  return doc;
}

function parseNode(raw: unknown, path: string): LessonNode {
  if (!isRecord(raw)) throw new LessonError(t('lessons.check.mustBeObject', { field: path }));
  const node: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(raw)) {
    if (key === 'children') {
      if (!Array.isArray(value)) throw new LessonError(t('lessons.check.mustBeList', { field: `${path}.children` }));
      node.children = value.map((child, i) => parseNode(child, `${path}.children[${i}]`));
    } else if (STRING_NODE_KEYS.has(key)) {
      if (typeof value !== 'string') throw new LessonError(t('lessons.check.mustBeText', { field: `${path}.${key}` }));
      node[key] = value;
    } else {
      throw new LessonError(t('lessons.check.unknownField', { key, path }));
    }
  }
  return node as unknown as LessonNode;
}

/** Port of rendering.Validate. Returns French messages; empty means valid. */
export function validateLesson(doc: LessonDocument): string[] {
  const errors: string[] = [];
  if (doc.schemaVersion !== 1 || doc.status !== 'unreviewed') errors.push(t('lessons.check.schema'));
  if (doc.language !== 'fr' && doc.language !== 'ar') errors.push(t('lessons.check.language'));
  if (byteLength(doc.title) === 0 || byteLength(doc.title) > 240) errors.push(t('lessons.check.title'));
  if (byteLength(doc.objective) > 1000) errors.push(t('lessons.check.objective'));
  if (doc.blocks.length === 0) errors.push(t('lessons.check.empty'));
  let count = 0;
  const walk = (nodes: LessonNode[], parent: string, depth: number, path: string): string | null => {
    if (depth > MAX_DEPTH) return t('lessons.check.tooDeep');
    for (const [i, n] of nodes.entries()) {
      const at = `${path}[${i}] (${n.type})`;
      if (++count > MAX_NODES) return t('lessons.check.tooManyNodes', { max: MAX_NODES });
      if (byteLength(n.text ?? '') > 8000 || byteLength(n.title ?? '') > 240 || byteLength(n.alt ?? '') > 1000 || byteLength(n.caption ?? '') > 1000) return t('lessons.check.textTooLong', { at });
      if (n.lang && n.lang !== 'ar' && n.lang !== 'fr') return t('lessons.check.nodeLanguage', { at });
      if (n.dir && n.dir !== 'rtl' && n.dir !== 'ltr' && n.dir !== 'auto') return t('lessons.check.direction', { at });
      if (INLINE.has(n.type) !== INLINE_PARENTS.has(parent)) return t('lessons.check.forbiddenIn', { at, type: n.type, parent });
      if (parent === 'list' && n.type !== 'item') return t('lessons.check.listItems', { at });
      if (n.type === 'item' && parent !== 'list') return t('lessons.check.itemOutside', { at });
      if (n.type === 'solution' && parent !== 'exercise') return t('lessons.check.solutionOutside', { at });
      if ((n.src && n.type !== 'image') || (n.alt && n.type !== 'image') || (n.caption && n.type !== 'image') || (n.videoId && n.type !== 'youtube')) return t('lessons.check.mediaAttributes', { at });
      const children = n.children ?? [];
      if (LEAVES.has(n.type)) {
        if (children.length > 0 || !n.text) return n.type === 'text' ? t('lessons.check.emptyText', { at }) : t('lessons.check.emptyFormula', { at });
        if ((n.type === 'math' || n.type === 'displayMath') && byteLength(n.text) > 2000) return t('lessons.check.formulaTooLong', { at });
      } else if (CONTAINERS.has(n.type)) {
        if (n.text || children.length === 0) return t('lessons.check.emptyContent', { at });
      } else if (n.type === 'image') {
        if (!isAllowedImageSrc(n.src ?? '') || BLANK.test(n.alt ?? '') || children.length > 0 || n.text) return t('lessons.check.image', { at });
      } else if (n.type === 'youtube') {
        if ((n.videoId && !YOUTUBE_ID.test(n.videoId)) || children.length > 0 || n.text) return t('lessons.check.youtube', { at });
      } else {
        return t('lessons.check.unsupportedType', { at });
      }
      const nested = walk(children, n.type, depth + 1, `${at}.children`);
      if (nested) return nested;
    }
    return null;
  };
  if (doc.blocks.length > 0) {
    const nested = walk(doc.blocks, 'document', 0, 'blocks');
    if (nested) errors.push(nested);
  }
  return errors;
}

/** Serialise with Go field order and the same 2-space layout as the fixtures. */
export function serializeLesson(doc: LessonDocument): string {
  const node = (n: LessonNode): Record<string, unknown> => {
    const out: Record<string, unknown> = {};
    for (const key of NODE_KEYS) {
      const value = n[key];
      if (key === 'children') {
        if (n.children && n.children.length > 0) out.children = n.children.map(node);
      } else if (value) {
        out[key] = value;
      }
    }
    return out;
  };
  const ordered = {
    schemaVersion: doc.schemaVersion,
    title: doc.title,
    language: doc.language,
    status: doc.status,
    objective: doc.objective,
    blocks: doc.blocks.map(node),
  };
  const json = JSON.stringify(ordered, null, 2);
  if (byteLength(json) > MAX_DOCUMENT_BYTES) throw new LessonError(t('lessons.check.tooLarge'));
  return json;
}

/**
 * Returns the first key repeated within one object, or null. Call only on
 * text JSON.parse accepted. Keys compare after unescaping, as in Go.
 */
export function findDuplicateKey(source: string): string | null {
  // One entry per open container: the keys seen so far, or null for an array.
  const stack: (Set<string> | null)[] = [];
  for (let i = 0; i < source.length; i++) {
    const ch = source[i];
    if (ch === '{') stack.push(new Set());
    else if (ch === '[') stack.push(null);
    else if (ch === '}' || ch === ']') stack.pop();
    else if (ch === '"') {
      const start = i;
      for (i++; source[i] !== '"'; i++) if (source[i] === '\\') i++;
      const keys = stack[stack.length - 1];
      if (!keys) continue;
      let j = i + 1;
      while (' \t\n\r'.includes(source[j])) j++;
      if (source[j] !== ':') continue; // a string value, not a key
      const key = JSON.parse(source.slice(start, i + 1)) as string;
      if (keys.has(key)) return key;
      keys.add(key);
    }
  }
  return null;
}

const INTEGER = /^-?(0|[1-9][0-9]*)$/;
const INT64_MIN = -(2n ** 63n);
const INT64_MAX = 2n ** 63n - 1n;

/**
 * Returns the first number literal that is not a plain integer within Go's
 * int (int64) range, or null. Call only on text JSON.parse accepted. The only
 * numeric lesson field is schemaVersion (a Go int); a number anywhere else is
 * already a type error on both sides, so checking every literal changes no
 * verdict and needs no path tracking.
 */
export function findNonIntegerNumber(source: string): string | null {
  for (let i = 0; i < source.length; i++) {
    const ch = source[i];
    if (ch === '"') {
      for (i++; source[i] !== '"'; i++) if (source[i] === '\\') i++;
    } else if (ch === '-' || (ch >= '0' && ch <= '9')) {
      const start = i;
      while (i + 1 < source.length && '0123456789eE.+-'.includes(source[i + 1])) i++;
      const lexeme = source.slice(start, i + 1);
      if (!INTEGER.test(lexeme)) return lexeme;
      const value = BigInt(lexeme);
      if (value < INT64_MIN || value > INT64_MAX) return lexeme;
    }
  }
  return null;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
