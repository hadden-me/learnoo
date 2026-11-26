// /studio/courses/{id}: the course's latest revision. A draft opens in the
// course editor; a course without an open draft offers to start one (steps
// that can no longer be attached are listed, never silently lost); a
// submitted revision (LMS-B04) is shown as a summary. Another user's course
// is reported exactly like a missing one, as the server does.
import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import type { Chapter, Subject } from '../api/client';
import { courseApi, findOwnCourse, type Course, type CourseRevision, type DroppedStep, type Level } from '../api/courses';
import { describeCourseError } from '../api/courseMessages';
import { CatalogueName, entryLabel } from '../catalogue';
import { setFlash, takeFlash, useStudio } from '../context';
import { COURSE_STATUS_LABELS, courseState, courseTitle } from '../courses/courseLabels';
import { CourseDraftEditor } from '../courses/CourseDraftEditor';
import { LANGUAGE_LABELS, formatDateTime } from '../labels';
import { BASE, navigate } from '../router';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';

type Loaded = {
  course: Course;
  revision: CourseRevision | null;
  subject: Subject;
  chapters: Chapter[];
  levels: Level[];
  lessonTitles: Map<number, string>;
};

export function CourseEditorPage({ courseId }: { courseId: number }) {
  const { api } = useStudio();
  const courses = useMemo(() => courseApi(api), [api]);
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(takeFlash);
  const [dropped, setDropped] = useState<{ steps: DroppedStep[]; revision: CourseRevision } | null>(null);
  const [generation, setGeneration] = useState(0);
  const [savedTitle, setSavedTitle] = useState<string | null>(null);

  const reload = useCallback(
    async (message?: string) => {
      setError(null);
      try {
        const course = await findOwnCourse(courses, courseId);
        if (!course) {
          setError('Cours introuvable : il n’existe pas ou ne vous appartient pas.');
          return;
        }
        const [subjects, levels, revision, lessons] = await Promise.all([
          api.subjects(),
          courses.levels(),
          course.latestRevision ? courses.revision(course.latestRevision.id) : Promise.resolve(null),
          api.myLessons(),
        ]);
        const subject = subjects.find((s) => s.id === course.subjectId);
        if (!subject) throw new Error('subject');
        const chapters = (await api.chapters(subject.code)).chapters;
        const lessonTitles = new Map(lessons.filter((l) => l.latestRevision).map((l) => [l.id, l.latestRevision!.title]));
        setLoaded({ course, revision, subject, chapters, levels, lessonTitles });
        setSavedTitle(null);
        setGeneration((g) => g + 1);
        if (message) setNotice(message);
      } catch (e) {
        setError(describeCourseError(e));
      }
    },
    [api, courses, courseId],
  );

  useEffect(() => {
    void reload();
  }, [reload]);

  if (error)
    return (
      <main className="page" id="main">
        <p className="breadcrumb">
          <Link to={`${BASE}/courses`}>Mes cours</Link>
        </p>
        <h1 tabIndex={-1}>Cours</h1>
        <div className="banner error" role="alert">
          <p>{error}</p>
          <button type="button" onClick={() => void reload()}>
            Réessayer
          </button>
        </div>
      </main>
    );
  if (!loaded)
    return (
      <main className="page" id="main">
        <p role="status">Chargement du cours…</p>
      </main>
    );

  const { course, revision, subject } = loaded;
  const state = courseState(course);
  const dir = course.language === 'ar' ? 'rtl' : 'ltr';
  const toList = (message: string) => {
    setFlash(message);
    navigate(`${BASE}/courses`, { force: true });
  };

  let body: ReactNode;
  if (revision?.status === 'draft') {
    body = (
      <CourseDraftEditor
        key={`${revision.id}-${generation}`}
        course={course}
        revision={revision}
        subject={subject}
        chapters={loaded.chapters}
        levels={loaded.levels}
        lessonTitles={loaded.lessonTitles}
        notice={dropped && dropped.revision.id === revision.id ? <DroppedNotice dropped={dropped.steps} revision={dropped.revision} /> : null}
        onDiscarded={() => toList('Brouillon du cours supprimé.')}
        onDeleted={() => toList('Cours supprimé. Les leçons et les images n’ont pas été touchées.')}
        onLeftDraft={() => void reload('Ce brouillon a quitté l’état de brouillon.')}
        onSaved={(saved) => setSavedTitle(saved.title)}
      />
    );
  } else {
    body = (
      <NoDraft
        course={course}
        revision={revision}
        onDraft={(created) => {
          setDropped({ steps: created.droppedSteps ?? [], revision: created });
          void reload('Nouveau brouillon créé : vous pouvez le modifier.');
        }}
        onDeleted={() => toList('Cours supprimé. Les leçons et les images n’ont pas été touchées.')}
      />
    );
  }

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={`${BASE}/courses`}>Mes cours</Link>
      </p>
      <h1 tabIndex={-1} lang={course.language} dir={dir}>
        {courseTitle(savedTitle ?? revision?.title ?? course.latestRevision?.title, course.id)}
      </h1>
      <p className="lesson-context">
        Cours · <CatalogueName label={entryLabel(subject, course.language)} /> · {LANGUAGE_LABELS[course.language]} · <span className={`state state-${state.tone}`}>{state.label}</span>
        {course.publishedRevisionId !== null && state.tone !== 'published' && <> · une version antérieure est publiée</>}
      </p>
      {notice && (
        <p className="banner success" role="status">
          {notice}
        </p>
      )}
      {body}
    </main>
  );
}

/** Steps of the copied revision that the new draft could not keep (another author's lesson no longer attachable at its revision). */
function DroppedNotice({ dropped, revision }: { dropped: DroppedStep[]; revision: CourseRevision }) {
  if (dropped.length === 0) return null;
  const moduleTitle = (key: string) => revision.modules.find((m) => m.key === key)?.title;
  return (
    <section className="status-card warning" aria-labelledby="dropped-heading">
      <h2 id="dropped-heading">
        {dropped.length} étape{dropped.length > 1 ? 's n’ont' : ' n’a'} pas été reprise{dropped.length > 1 ? 's' : ''} dans ce brouillon
      </h2>
      <p>La leçon d’un autre auteur n’est plus rattachable à la révision épinglée (elle n’est plus publiée, ou cette révision a été retirée). Ajoutez-la de nouveau avec une révision publiable, ou remplacez l’étape.</p>
      <ul>
        {dropped.map((s) => (
          <li key={`${s.moduleKey}-${s.position}`}>
            « {s.title} » (leçon n° {s.lessonId}){moduleTitle(s.moduleKey) ? `, module « ${moduleTitle(s.moduleKey)} »` : ''}, position {s.position}
          </li>
        ))}
      </ul>
    </section>
  );
}

/**
 * No open draft: the course's only draft was discarded, or its latest
 * revision was submitted (LMS-B04 adds review, publication and amendments
 * here). Starting a draft copies the latest revision.
 */
function NoDraft({ course, revision, onDraft, onDeleted }: { course: Course; revision: CourseRevision | null; onDraft: (created: CourseRevision) => void; onDeleted: () => void }) {
  const { api, expired } = useStudio();
  const courses = useMemo(() => courseApi(api), [api]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirm, setConfirm] = useState(false);

  const start = async () => {
    setBusy(true);
    setError(null);
    try {
      onDraft(await courses.startDraft(course.id));
    } catch (e) {
      setError(describeCourseError(e));
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    setConfirm(false);
    setError(null);
    try {
      await courses.deleteCourse(course.id);
      onDeleted();
    } catch (e) {
      setError(describeCourseError(e));
    }
  };

  return (
    <section className="status-card" aria-labelledby="nodraft-heading">
      {revision ? (
        <>
          <h2 id="nodraft-heading">
            Version {revision.number} · {COURSE_STATUS_LABELS[revision.status]}
          </h2>
          <p>Cette version du cours a quitté l’état de brouillon : elle ne peut plus être modifiée.</p>
          {revision.submittedAt && <p className="hint">Soumise le {formatDateTime(revision.submittedAt)}.</p>}
        </>
      ) : (
        <>
          <h2 id="nodraft-heading">Aucun brouillon en cours</h2>
          <p>Le brouillon de ce cours a été supprimé. Créez un nouveau brouillon pour reprendre la rédaction, ou supprimez le cours s’il n’a jamais été soumis.</p>
        </>
      )}
      <div className="actions">
        {(!revision || revision.status === 'changes_requested') && (
          <button type="button" className="primary" onClick={() => void start()} disabled={busy || expired}>
            {revision ? 'Créer la version corrigée' : 'Créer un brouillon'}
          </button>
        )}
        {!revision && (
          <button type="button" className="danger" onClick={() => setConfirm(true)} disabled={busy || expired}>
            Supprimer le cours
          </button>
        )}
      </div>
      {error && (
        <p className="banner error" role="alert">
          {error}
        </p>
      )}
      {confirm && (
        <Dialog title="Supprimer définitivement le cours ?" onCancel={() => setConfirm(false)} cancelLabel="Garder le cours" actions={[{ label: 'Supprimer le cours', kind: 'danger', onClick: () => void remove() }]}>
          <p>Le cours sera supprimé du serveur. Cette action est définitive. Les leçons et les images ne sont pas supprimées. Un cours déjà soumis ne peut pas être supprimé.</p>
        </Dialog>
      )}
    </section>
  );
}
