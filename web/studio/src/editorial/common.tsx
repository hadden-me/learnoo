// Building blocks shared by the reviewer, publisher, administrator and
// error-report pages: loading state, the canonical preview of a stored
// revision, the "no access" page and a confirmation step for decisions.
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { isApiError, type MediaRoute } from '../api/client';
import { editorialApi } from '../api/editorial';
import { describeEditorialError } from '../api/editorialMessages';
import { useStudio } from '../context';
import { disableVideos, loadKatex, typeset } from '../preview';
import { BASE } from '../router';
import { Link } from '../ui/Link';

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

/** Runs `load` on mount and whenever `reload` is called; errors become French messages. */
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

/** Subject codes by ID from the catalogue; empty when the account may not read it (administrators, publishers). */
export function useSubjectCodes(): Map<number, string> {
  const { api } = useStudio();
  const [codes, setCodes] = useState(() => new Map<number, string>());
  useEffect(() => {
    api.subjects().then(
      (subjects) => setCodes(new Map(subjects.map((s) => [s.id, s.code]))),
      () => undefined,
    );
  }, [api]);
  return codes;
}

export const subjectLabel = (codes: Map<number, string>, id: number) => codes.get(id) ?? `matière n° ${id}`;

/**
 * The server's canonical rendering (POST /api/preview) of a stored
 * revision's content, typeset with the application's KaTeX, as the reader
 * will show it. Read-only.
 */
export function CanonicalPreview({ content, testId, mediaRoute }: { content: unknown; testId: string; mediaRoute: MediaRoute }) {
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
      {!ready && !error && <p role="status">Rendu de la leçon…</p>}
      {error && (
        <p className="banner error" role="alert">
          Aperçu impossible. {error}
        </p>
      )}
      <div ref={ref} className="lesson-preview" data-testid={testId} />
    </>
  );
}

export function LoadingPage({ label = 'Chargement…' }: { label?: string }) {
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
      <h1 tabIndex={-1}>Accès refusé</h1>
      <p>
        Votre compte n’a pas accès à l’espace « {space} ». Le serveur refuse ces actions aux comptes qui n’ont pas la permission correspondante ; demandez-la à un administrateur si
        vous en avez besoin.
      </p>
      <p>
        <Link to={`${BASE}/`}>Revenir à l’accueil du Studio</Link>
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
