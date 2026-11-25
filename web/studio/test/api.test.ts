import { describe, expect, it, vi } from 'vitest';
import { Api, ApiError } from '../src/api/client';
import { describeError } from '../src/api/messages';
import { buildFields, discardOnServer, saveToServer, submitToServer } from '../src/drafts';

type Call = { url: string; method: string; headers: Record<string, string>; body: unknown };

/** A fake server: answers each request with the next scripted response and records what was sent. */
function fakeServer(responses: Array<[number, unknown, Record<string, string>?]>) {
  const calls: Call[] = [];
  const fetch = vi.fn(async (url: string, init: RequestInit) => {
    calls.push({ url, method: init.method ?? 'GET', headers: init.headers as Record<string, string>, body: init.body });
    const next = responses.shift();
    if (!next) throw new TypeError('Failed to fetch');
    const [status, body, headers] = next;
    return new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });
  });
  return { fetch: fetch as unknown as typeof globalThis.fetch, calls };
}

const session = { user: { id: 7, email: 'test-author@example.test', displayName: 'TEST' }, csrfToken: 'tok-1', expiresAt: '2026-11-25T09:00:00Z' };
const err = (code: string, message = code) => ({ error: { code, message } });
const revision = (version: number) => ({ id: 12, lessonId: 4, number: 1, status: 'draft', version, content: {}, mediaIds: [] });
const fields = { content: { schemaVersion: 1 }, prerequisites: '', chapterId: null, mediaIds: [] };

describe('API client', () => {
  it('fetches the CSRF token before the first write and sends it on unsafe requests only', async () => {
    const server = fakeServer([[200, session], [200, revision(2)], [200, revision(2)]]);
    const api = new Api({ fetch: server.fetch });
    await api.saveRevision(12, 1, fields);
    await api.revision(12);
    expect(server.calls.map((c) => `${c.method} ${c.url}`)).toEqual(['GET /api/auth/session', 'PUT /api/revisions/12', 'GET /api/revisions/12']);
    expect(server.calls[1].headers['X-CSRF-Token']).toBe('tok-1');
    expect(server.calls[1].headers['Content-Type']).toBe('application/json');
    expect(JSON.parse(server.calls[1].body as string)).toEqual({ expectedVersion: 1, ...fields });
    expect(server.calls[2].headers['X-CSRF-Token']).toBeUndefined();
  });

  it('refreshes a rejected CSRF token once and repeats the request', async () => {
    const server = fakeServer([[200, session], [403, err('csrf_rejected')], [200, { ...session, csrfToken: 'tok-2' }], [200, revision(2)]]);
    const api = new Api({ fetch: server.fetch });
    await expect(api.saveRevision(12, 1, fields)).resolves.toMatchObject({ version: 2 });
    expect(server.calls[3].headers['X-CSRF-Token']).toBe('tok-2');
  });

  it('reports 401 to the shell and throws a typed error', async () => {
    const onUnauthenticated = vi.fn();
    const server = fakeServer([[401, err('unauthenticated')]]);
    const api = new Api({ fetch: server.fetch, onUnauthenticated });
    const error = await api.myLessons().catch((e) => e);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 401, code: 'unauthenticated' });
    expect(onUnauthenticated).toHaveBeenCalledOnce();
  });

  it('uploads the raw image body with its own type, not multipart', async () => {
    const server = fakeServer([[200, session], [201, { id: 5, url: '/api/media/5' }]]);
    const api = new Api({ fetch: server.fetch });
    const file = new Blob([new Uint8Array([0x89, 0x50])], { type: 'image/png' });
    await api.uploadMedia(file);
    expect(server.calls[1].headers['Content-Type']).toBe('image/png');
    expect(server.calls[1].body).toBe(file);
  });

  it('keeps Retry-After, and maps errors without a JSON body or network failures to codes', async () => {
    const server = fakeServer([[200, session], [503, err('busy'), { 'Retry-After': '5' }]]);
    const api = new Api({ fetch: server.fetch });
    const busy = await api.uploadMedia(new Blob([], { type: 'image/png' })).catch((e) => e);
    expect(busy).toMatchObject({ code: 'busy', retryAfter: 5 });
    expect(describeError(busy)).toContain('(5 s)');
    const network = await api.revision(1).catch((e) => e);
    expect(network).toMatchObject({ status: 0, code: 'network' });
    const html = vi.fn(async () => new Response('<html>Bad gateway</html>', { status: 502 }));
    const proxied = await new Api({ fetch: html as unknown as typeof fetch }).revision(1).catch((e) => e);
    expect(proxied).toMatchObject({ status: 502, code: 'http_502' });
  });

  it('explains trailing_data and includes the validator reason for invalid content', () => {
    expect(describeError(new ApiError(422, 'trailing_data', 'x'))).toMatch(/données après sa fin.*PNG/);
    expect(describeError(new ApiError(422, 'invalid_content', 'blocks[0]: empty text'))).toContain('blocks[0]: empty text');
    expect(describeError(new Error('boom'))).toBe('Erreur inattendue dans le Studio.');
  });
});

describe('saving drafts', () => {
  it('a stale save is a conflict, never an overwrite', async () => {
    const server = fakeServer([[200, session], [409, err('version_conflict')]]);
    const outcome = await saveToServer(new Api({ fetch: server.fetch }), { kind: 'revision', revisionId: 12, version: 3 }, fields);
    expect(outcome).toEqual({ kind: 'conflict' });
    expect(server.calls).toHaveLength(2);
  });

  it('repeats a save once when the database asked for a retry, then reports it', async () => {
    const ok = fakeServer([[200, session], [409, err('retry')], [200, revision(4)]]);
    expect(await saveToServer(new Api({ fetch: ok.fetch }), { kind: 'revision', revisionId: 12, version: 3 }, fields)).toMatchObject({ kind: 'saved', revision: { version: 4 } });
    const twice = fakeServer([[200, session], [409, err('retry')], [409, err('retry')]]);
    const outcome = await saveToServer(new Api({ fetch: twice.fetch }), { kind: 'revision', revisionId: 12, version: 3 }, fields);
    expect(outcome.kind).toBe('failed');
  });

  it('classifies an expired session separately so the edits are kept', async () => {
    const server = fakeServer([[200, session], [401, err('unauthenticated')]]);
    expect(await saveToServer(new Api({ fetch: server.fetch }), { kind: 'revision', revisionId: 12, version: 3 }, fields)).toEqual({ kind: 'expired' });
  });

  it('creates a lesson, or a draft of an existing lesson, with the right endpoint and body', async () => {
    const server = fakeServer([[200, session], [201, revision(1)], [201, revision(1)]]);
    const api = new Api({ fetch: server.fetch });
    await saveToServer(api, { kind: 'lesson', params: { subjectId: 1, subjectCode: 'maths', language: 'ar', translationOf: 9 } }, fields);
    await saveToServer(api, { kind: 'draft', lessonId: 4 }, fields);
    expect(server.calls[1].url).toBe('/api/lessons');
    expect(JSON.parse(server.calls[1].body as string)).toEqual({ subjectId: 1, language: 'ar', translationOf: 9, ...fields });
    expect(server.calls[2].url).toBe('/api/lessons/4/drafts');
  });

  it('submits and discards with the expected version', async () => {
    const server = fakeServer([[200, session], [409, err('version_conflict')], [204, null]]);
    const api = new Api({ fetch: server.fetch });
    expect(await submitToServer(api, 12, 3)).toEqual({ kind: 'conflict' });
    expect(JSON.parse(server.calls[1].body as string)).toEqual({ expectedVersion: 3 });
    expect(await discardOnServer(api, 12, 3)).toBe(true);
    expect(server.calls[2]).toMatchObject({ method: 'DELETE', url: '/api/revisions/12?expectedVersion=3' });
  });

  it('refuses to build a request from an invalid document', () => {
    const empty = buildFields({ doc: { type: 'doc', content: [{ type: 'paragraph' }] }, title: 'TEST', objective: '', language: 'fr', prerequisites: '', chapterId: null, mediaIds: [] });
    expect(empty.fields).toBeNull();
    expect(empty.errors).toEqual(['Le document est vide.']);
    const ok = buildFields({ doc: { type: 'doc', content: [{ type: 'paragraph', content: [{ type: 'text', text: 'x' }] }] }, title: 'TEST', objective: '', language: 'fr', prerequisites: 'p', chapterId: 2, mediaIds: [5, 5, 6] });
    expect(ok.fields).toMatchObject({ prerequisites: 'p', chapterId: 2, mediaIds: [5, 6], content: { title: 'TEST', status: 'unreviewed' } });
    const long = buildFields({ doc: { type: 'doc', content: [{ type: 'paragraph', content: [{ type: 'text', text: 'x' }] }] }, title: 'TEST', objective: '', language: 'fr', prerequisites: 'é'.repeat(2001), chapterId: null, mediaIds: [] });
    expect(long.errors[0]).toMatch(/2 000/);
  });
});
