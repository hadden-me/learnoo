// "Mes cours": the author's own courses from GET /api/courses?mine=1, a page
// of 100 at a time (cursor). Filtering by state and searching titles apply
// to the courses already loaded; "Afficher plus" loads the next page.
import { useCallback, useEffect, useId, useMemo, useState } from 'react';
import type { Subject } from '../api/client';
import { courseApi, type Course } from '../api/courses';
import { describeCourseError } from '../api/courseMessages';
import { CatalogueName, entryLabel } from '../catalogue';
import { takeFlash, useStudio } from '../context';
import { COURSE_FILTERS, courseState, courseTitle, type CourseStateFilter } from '../courses/courseLabels';
import { fold } from '../courses/outline';
import { LANGUAGE_LABELS, formatDateTime } from '../labels';
import { BASE } from '../router';
import { Link } from '../ui/Link';

export function CourseList() {
  const { api } = useStudio();
  const courses = useMemo(() => courseApi(api), [api]);
  const ids = { search: useId(), filter: useId() };
  const [items, setItems] = useState<Course[] | null>(null);
  const [subjects, setSubjects] = useState<Subject[]>([]);
  const [next, setNext] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [flash] = useState(takeFlash);
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState<CourseStateFilter>('all');

  const load = useCallback(
    async (cursor: string | null) => {
      setLoading(true);
      setError(null);
      try {
        const [page, s] = await Promise.all([courses.myCourses(cursor), cursor === null ? api.subjects() : Promise.resolve(null)]);
        if (s) setSubjects(s);
        setItems((prev) => (cursor === null ? page.items : [...(prev ?? []), ...page.items]));
        setNext(page.nextCursor);
      } catch (e) {
        setError(describeCourseError(e));
      } finally {
        setLoading(false);
      }
    },
    [api, courses],
  );

  useEffect(() => {
    void load(null);
  }, [load]);

  const shown = useMemo(() => {
    if (!items) return [];
    const q = fold(query.trim());
    return items.filter((c) => {
      const status = c.latestRevision?.status ?? 'none';
      if (filter !== 'all' && status !== filter) return false;
      return q === '' || fold(c.latestRevision?.title ?? '').includes(q);
    });
  }, [items, query, filter]);

  const subjectOf = (id: number) => subjects.find((s) => s.id === id);
  const filtered = query.trim() !== '' || filter !== 'all';

  return (
    <main className="page" id="main">
      <div className="page-head">
        <h1 tabIndex={-1}>Mes cours</h1>
        <Link to={`${BASE}/courses/new`} className="button primary">
          Nouveau cours
        </Link>
      </div>
      <p className="hint">Un cours regroupe vos modules et les leçons qui les composent, rattachés à une ou plusieurs éditions du programme officiel.</p>
      {flash && (
        <p className="banner success" role="status">
          {flash}
        </p>
      )}
      {error && (
        <div className="banner error" role="alert">
          <p>{error}</p>
          <button type="button" onClick={() => void load(items === null ? null : next)}>
            Réessayer
          </button>
        </div>
      )}
      {items === null && !error && <p role="status">Chargement des cours…</p>}
      {items && items.length === 0 && <p>Vous n’avez pas encore de cours. Créez-en un pour commencer.</p>}
      {items && items.length > 0 && (
        <>
          <div className="find-lesson" role="search" aria-label="Filtrer mes cours">
            <div className="fields">
              <label htmlFor={ids.search}>Rechercher un titre</label>
              <input id={ids.search} type="search" value={query} onChange={(e) => setQuery(e.target.value)} />
            </div>
            <div className="fields">
              <label htmlFor={ids.filter}>État</label>
              <select id={ids.filter} className="inline-select" value={filter} onChange={(e) => setFilter(e.target.value as CourseStateFilter)}>
                {COURSE_FILTERS.map(([value, label]) => (
                  <option key={value} value={value}>
                    {label}
                  </option>
                ))}
              </select>
            </div>
          </div>
          <p role="status" className="hint">
            {filtered ? `${shown.length} cours sur ${items.length} chargés correspondent.` : `${items.length} cours chargés.`}
            {next !== null ? ' D’autres cours peuvent être chargés en bas de la liste.' : ''}
          </p>
          {shown.length === 0 ? (
            <div>
              <p>Aucun cours chargé ne correspond à cette recherche.</p>
              <button
                type="button"
                onClick={() => {
                  setQuery('');
                  setFilter('all');
                }}
              >
                Effacer les filtres
              </button>
            </div>
          ) : (
            <ul className="lesson-list" aria-label="Cours dont vous êtes le propriétaire">
              {shown.map((course) => {
                const state = courseState(course);
                const subject = subjectOf(course.subjectId);
                const latest = course.latestRevision;
                return (
                  <li key={course.id} className="lesson-card">
                    <h2 lang={course.language} dir={course.language === 'ar' ? 'rtl' : 'ltr'}>
                      <Link to={`${BASE}/courses/${course.id}`}>{courseTitle(latest?.title, course.id)}</Link>
                    </h2>
                    <p className="lesson-meta">
                      <span className={`state state-${state.tone}`}>{state.label}</span>
                      {course.publishedRevisionId !== null && state.tone !== 'published' && <span className="state state-published">Une version est publiée</span>}
                      {course.archived && <span className="state">Archivé</span>}
                    </p>
                    <p className="lesson-context">
                      {subject ? <CatalogueName label={entryLabel(subject, course.language)} /> : `Matière n° ${course.subjectId}`} · {LANGUAGE_LABELS[course.language]}
                      {latest && (
                        <>
                          {' '}
                          · version {latest.number} du cours · modifié le {formatDateTime(latest.updatedAt)}
                        </>
                      )}
                    </p>
                  </li>
                );
              })}
            </ul>
          )}
          {next !== null && (
            <button type="button" onClick={() => void load(next)} disabled={loading}>
              {loading ? 'Chargement…' : 'Afficher plus de cours'}
            </button>
          )}
        </>
      )}
    </main>
  );
}
