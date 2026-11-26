// Building blocks shared by the reviewer, publisher, administrator and
// error-report pages: loading state, the canonical preview of a stored
// revision, the "no access" page and a confirmation step for decisions.
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { isApiError, type MediaRoute, type Subject } from '../api/client';
import { editorialApi } from '../api/editorial';
import { CatalogueName, entryLabel, type CatalogueLabel } from '../catalogue';
import { describeEditorialError } from '../api/editorialMessages';
import { useStudio } from '../context';
import { disableVideos, loadKatex, typeset } from '../preview';
import { BASE } from '../router';
import { Link } from '../ui/Link';
import { locale, t } from '../i18n';

export const dirOf = (language: string) => (language === 'ar' ? 'rtl' : 'ltr');

/** The editorial calls bound to the shell's API client. */
export function useEditorial() {
  const { api } = useStudio();
  return useMemo(() => editorialApi(api), [api]);
}

/** Request sequence guard: only the most recently started request may apply its result. */
export function latestOnly() {
  let counter = 0;
  return { next: () => ++counter, isCurrent: (ticket: number) => ticket === counter };
}

export type Loaded<T> = { state: 'loading' } | { state: 'ready'; data: T } | { state: 'error'; message: string; notFound: boolean };

/** Runs `load` on mount and whenever `reload` is called; errors become interface-language messages. */
export function useLoad<T>(load: () => Promise<T>, deps: unknown[]): [Loaded<T>, () => Promise<void>] {
  const [value, setValue] = useState<Loaded<T>>({ state: 'loading' });
  const run = useCallback(load, deps);
  const [latest] = useState(latestOnly);
  const reload = useCallback(async () => {
    const ticket = latest.next();
    let next: Loaded<T>;
    try {
      next = { state: 'ready', data: await run() };
    } catch (e) {
      next = { state: 'error', message: describeEditorialError(e), notFound: isApiError(e, 'not_found') };
    }
    // A slower, older request (for example the previous filter) never overwrites a newer one.
    if (latest.isCurrent(ticket)) setValue(next);
  }, [run, latest]);
  useEffect(() => {
    void reload();
  }, [reload]);
  return [value, reload];
}

/** Catalogue subjects by ID; empty when the account may not read the catalogue (administrators, publishers). */
export function useSubjects(): Map<number, Subject> {
  const { api } = useStudio();
  const [subjects, setSubjects] = useState(() => new Map<number, Subject>());
  useEffect(() => {
    api.subjects().then(
      (list) => setSubjects(new Map(list.map((s) => [s.id, s]))),
      () => undefined,
    );
  }, [api]);
  return subjects;
}

/** A subject's name for the editorial pages: in the interface language, else the other, else code, else its ID. */
export function subjectName(subjects: Map<number, Subject>, id: number): CatalogueLabel {
  const s = subjects.get(id);
  return s ? entryLabel(s, locale()) : { text: t('shell.subjectNumber', { id }), lang: null };
}

export const subjectLabel = (subjects: Map<number, Subject>, id: number) => subjectName(subjects, id).text;

export function SubjectName({ subjects, id }: { subjects: Map<number, Subject>; id: number }) {
  return <CatalogueName label={subjectName(subjects, id)} />;
}

/**
 * The server's canonical rendering (POST /api/preview) of a stored
 * revision's content, typeset with the application's KaTeX, as the reader
 * will show it. Read-only.
 */
export function CanonicalPreview({ content, testId, mediaRoute }: { content: unknown; testId: string; mediaRoute: MediaRoute }) {
  // Nothing sent (a withdrawn or unpublished lesson): nothing is requested or rendered.
  if (content === null || content === undefined)
    return (
      <p className="hint" data-testid={testId}>
        {t('shell.contentUnavailable')}
      </p>
    );
  return <RenderedPreview content={content} testId={testId} mediaRoute={mediaRoute} />;
}

function RenderedPreview({ content, testId, mediaRoute }: { content: unknown; testId: string; mediaRoute: MediaRoute }) {
  const { api } = useStudio();
  const ref = useRef<HTMLDivElement>(null);
  const [error, setError] = useState<string | null>(null);
  const [ready, setReady] = useState(false);
  useEffect(() => {
    let cancelled = false;
    api.preview(content, mediaRoute).then(
      async (html) => {
        const root = ref.current;
        if (cancelled || !root) return;
        // Server-rendered, allowlisted lesson markup, as on the reader page.
        root.innerHTML = html;
        disableVideos(root);
        typeset(root, await loadKatex());
        setReady(true);
      },
      (e) => !cancelled && setError(describeEditorialError(e)),
    );
    return () => {
      cancelled = true;
    };
  }, [api, content, mediaRoute]);
  return (
    <>
      {!ready && !error && <p role="status">{t('shell.renderingLesson')}</p>}
      {error && (
        <p className="banner error" role="alert">
          {t('shell.previewFailed', { error })}
        </p>
      )}
      <div ref={ref} className="lesson-preview" data-testid={testId} />
    </>
  );
}

export function LoadingPage({ label = t('shell.loadingPage') }: { label?: string }) {
  return (
    <main className="page" id="main">
      <p role="status">{label}</p>
    </main>
  );
}

export function ErrorPage({ title, message, back }: { title: string; message: string; back: { to: string; label: string } }) {
  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>{title}</h1>
      <p className="banner error" role="alert">
        {message}
      </p>
      <p>
        <Link to={back.to}>{back.label}</Link>
      </p>
    </main>
  );
}

/** Shown for a space the server does not let this account use (it would refuse every request anyway). */
export function NoAccess({ space }: { space: string }) {
  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>{t('shell.accessDenied')}</h1>
      <p>{t('shell.accessDeniedText', { space })}</p>
      <p>
        <Link to={`${BASE}/`}>{t('shell.backHome')}</Link>
      </p>
    </main>
  );
}

/** A required or optional text field for a comment, reason or note, with its own error. */
export function NoteField({
  id,
  label,
  hint,
  value,
  onChange,
  error,
  fieldRef,
}: {
  id: string;
  label: string;
  hint: string;
  value: string;
  onChange: (v: string) => void;
  error: string | null;
  fieldRef?: React.Ref<HTMLTextAreaElement>;
}) {
  return (
    <div className="fields">
      <label htmlFor={id}>{label}</label>
      <p className="hint" id={`${id}-hint`}>
        {hint}
      </p>
      {error && (
        <p className="field-error" id={`${id}-error`}>
          {error}
        </p>
      )}
      <textarea
        id={id}
        ref={fieldRef}
        rows={4}
        dir="auto"
        value={value}
        aria-invalid={error ? true : undefined}
        aria-describedby={`${id}-hint${error ? ` ${id}-error` : ''}`}
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  );
}

/** A quoted, user-written text (comment, report, reason): plain text, direction from its content. */
export function Quote({ children }: { children: ReactNode }) {
  return (
    <blockquote className="quote" dir="auto">
      {children}
    </blockquote>
  );
}
