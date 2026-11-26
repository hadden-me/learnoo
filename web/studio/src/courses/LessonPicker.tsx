// Dialog that attaches a lesson to a module as a step, or changes the
// revision a step pins. Lessons come from the attachable-lessons search
// (the course's subject and language: the author's own lessons, or another
// author's published ones), revisions from eligible-revisions. The author's
// own lesson may pin any revision in a draft; only eligible revisions can be
// submitted with the course, and the picker says which.
import { useCallback, useEffect, useId, useRef, useState, type FormEvent } from 'react';
import type { AttachableLesson, CourseApi, EligibleRevision } from '../api/courses';
import { describeCourseError } from '../api/courseMessages';
import { STATUS_LABELS } from '../labels';
import type { Lang } from '../catalogue';
import { Dialog } from '../ui/Dialog';
import { LIMITS, type DraftStep } from './outline';

export type PickedStep = Omit<DraftStep, 'localId'>;

/** The revision a new step pins by default: the newest eligible one, else (own lessons) the newest. */
export function defaultRevision(revisions: EligibleRevision[]): EligibleRevision | null {
  return revisions.find((r) => r.eligible) ?? revisions[0] ?? null;
}

const revisionLabel = (r: EligibleRevision) => `Révision ${r.number} · ${STATUS_LABELS[r.status]} · ${r.eligible ? 'publiable dans un cours' : 'pas encore publiable dans un cours'}`;

export function LessonPicker({
  courses,
  courseId,
  language,
  taken,
  change,
  moduleTitle,
  onPick,
  onCancel,
}: {
  courses: CourseApi;
  courseId: number;
  language: Lang;
  /** Lessons already in the outline: a lesson appears once per course. */
  taken: Set<number>;
  /** Set to change the pinned revision of an existing step instead of adding one. */
  change?: DraftStep;
  moduleTitle: string;
  onPick: (step: PickedStep) => void;
  onCancel: () => void;
}) {
  const ids = { search: useId(), revision: useId(), title: useId(), results: useId() };
  const searchRef = useRef<HTMLInputElement>(null);
  const revisionRef = useRef<HTMLSelectElement>(null);
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<AttachableLesson[] | null>(null);
  const [next, setNext] = useState<string | null>(null);
  const [searching, setSearching] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [lesson, setLesson] = useState<{ lessonId: number; title: string; own: boolean } | null>(change ? { lessonId: change.lessonId, title: change.lessonTitle ?? change.title, own: change.ownLesson ?? false } : null);
  const [revisions, setRevisions] = useState<EligibleRevision[] | null>(null);
  const [revisionId, setRevisionId] = useState<number | null>(change?.revisionId ?? null);
  const [title, setTitle] = useState(change?.title ?? '');
  const [revisionsError, setRevisionsError] = useState<string | null>(null);
  const [revisionsAttempt, setRevisionsAttempt] = useState(0);

  const search = useCallback(
    async (q: string, cursor: string | null) => {
      setSearching(true);
      setError(null);
      try {
        const page = await courses.attachableLessons(courseId, q.trim(), cursor);
        setResults((prev) => (cursor === null ? page.items : [...(prev ?? []), ...page.items]));
        setNext(page.nextCursor);
      } catch (e) {
        setError(describeCourseError(e));
      } finally {
        setSearching(false);
      }
    },
    [courses, courseId],
  );

  useEffect(() => {
    if (!change) void search('', null);
  }, []);

  useEffect(() => {
    if (!lesson) return;
    let cancelled = false;
    setRevisions(null);
    setRevisionsError(null);
    courses.eligibleRevisions(lesson.lessonId).then(
      (list) => {
        if (cancelled) return;
        setRevisions(list);
        if (!change || !list.some((r) => r.id === change.revisionId)) setRevisionId(defaultRevision(list)?.id ?? null);
      },
      (e) => !cancelled && setRevisionsError(describeCourseError(e)),
    );
    return () => {
      cancelled = true;
    };
  }, [courses, lesson, revisionsAttempt]);

  // Once a lesson's revisions are listed, the revision choice is the next step.
  useEffect(() => {
    if (revisions && revisions.length > 0) revisionRef.current?.focus();
  }, [revisions]);

  const submitSearch = (event: FormEvent) => {
    event.preventDefault();
    event.stopPropagation();
    void search(query, null);
  };

  const chosen = revisions?.find((r) => r.id === revisionId) ?? null;
  const titleLength = [...title.trim()].length;
  const ready = lesson !== null && chosen !== null && titleLength > 0 && titleLength <= LIMITS.stepTitle;

  const pick = () => {
    if (!ready || !lesson || !chosen) return;
    onPick({ lessonId: lesson.lessonId, revisionId: chosen.id, title: title.trim(), revisionNumber: chosen.number, ownLesson: lesson.own, eligible: chosen.eligible, lessonTitle: lesson.title });
  };

  const dir = language === 'ar' ? 'rtl' : 'ltr';
  return (
    <Dialog
      title={change ? `Changer la révision de l’étape « ${change.title} »` : `Ajouter une leçon au module « ${moduleTitle || 'sans titre'} »`}
      onCancel={onCancel}
      initialFocus={change ? revisionRef : searchRef}
      actions={[{ label: change ? 'Changer la révision' : 'Ajouter l’étape', kind: 'primary', disabled: !ready, onClick: pick }]}
    >
      {!change && (
        <>
          <form className="picker-search" onSubmit={submitSearch} role="search">
            <label htmlFor={ids.search}>Rechercher une leçon de la matière et de la langue du cours</label>
            <div className="actions">
              <input ref={searchRef} id={ids.search} type="search" value={query} maxLength={100} lang={language} dir="auto" onChange={(e) => setQuery(e.target.value)} />
              <button type="submit" disabled={searching}>
                Rechercher
              </button>
            </div>
          </form>
          <p className="hint">Vos leçons (toutes leurs révisions) et les leçons publiées d’autres auteurs.</p>
          <div aria-live="polite">
            {searching && results === null && <p role="status">Recherche…</p>}
            {results && results.length === 0 && <p>Aucune leçon ne correspond. Créez d’abord la leçon dans « Mes leçons », ou vérifiez qu’elle est dans la matière et la langue du cours.</p>}
          </div>
          {results && results.length > 0 && (
            <ul className="picker-results" id={ids.results} aria-label="Leçons trouvées">
              {results.map((l) => {
                const already = taken.has(l.lessonId);
                const selected = lesson?.lessonId === l.lessonId;
                return (
                  <li key={l.lessonId}>
                    <button
                      type="button"
                      aria-pressed={selected}
                      disabled={already}
                      onClick={() => {
                        setLesson({ lessonId: l.lessonId, title: l.title, own: l.own });
                        setTitle((t) => (t.trim() === '' || t === lesson?.title ? l.title : t));
                      }}
                    >
                      <span lang={language} dir={dir}>
                        {l.title || `Leçon n° ${l.lessonId}`}
                      </span>
                      <span className="hint">
                        {' '}
                        · {l.own ? 'votre leçon' : 'leçon publiée d’un autre auteur'}
                        {already ? ' · déjà dans le cours' : ''}
                      </span>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
          {next !== null && (
            <button type="button" onClick={() => void search(query, next)} disabled={searching}>
              Afficher plus de leçons
            </button>
          )}
        </>
      )}
      {error && (
        <p className="banner error" role="alert">
          {error}
        </p>
      )}
      {lesson && (
        <div className="fields picked">
          <p>
            Leçon choisie :{' '}
            <strong lang={language} dir={dir}>
              {lesson.title || `Leçon n° ${lesson.lessonId}`}
            </strong>
          </p>
          <label htmlFor={ids.revision}>Révision à épingler</label>
          {revisionsError ? (
            <div className="banner error" role="alert">
              <p>Les révisions de cette leçon n’ont pas pu être chargées. {revisionsError}</p>
              <button type="button" onClick={() => setRevisionsAttempt((n) => n + 1)}>
                Réessayer
              </button>
            </div>
          ) : revisions === null ? (
            <p role="status">Chargement des révisions…</p>
          ) : revisions.length === 0 ? (
            <p>Cette leçon n’a aucune révision qui puisse être rattachée.</p>
          ) : (
            <select ref={revisionRef} id={ids.revision} value={revisionId ?? ''} onChange={(e) => setRevisionId(Number(e.target.value))}>
              {revisions.map((r) => (
                <option key={r.id} value={r.id}>
                  {revisionLabel(r)}
                </option>
              ))}
            </select>
          )}
          {chosen && !chosen.eligible && (
            <p className="hint">Cette révision peut figurer dans le brouillon, mais elle devra être approuvée et publiée avant la soumission du cours.</p>
          )}
          {!change && (
            <>
              <label htmlFor={ids.title}>Titre de l’étape dans le cours</label>
              <input id={ids.title} value={title} maxLength={LIMITS.stepTitle} lang={language} dir={dir} onChange={(e) => setTitle(e.target.value)} />
              <p className="hint">Ce titre est celui du plan du cours ; il est relu avec la structure du cours.</p>
            </>
          )}
        </div>
      )}
    </Dialog>
  );
}
