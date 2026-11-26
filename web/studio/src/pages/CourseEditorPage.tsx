// /studio/courses/{id}: the course's latest revision. A draft opens in the
// course editor, where it is submitted for review; a course without an open
// draft shows its latest revision's state (in review, blocked, changes
// requested with the feedback, approved, published, withdrawn) and offers to
// start a new draft (steps that can no longer be attached are listed, never
// silently lost). Suspension and archive are shown above, the newer lesson
// revision notice (amendments) and the archive request below. Another
// user's course is reported exactly like a missing one, as the server does.
import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import type { Chapter, Subject } from '../api/client';
import { courseApi, findOwnCourse, type Course, type CourseRevision, type DroppedStep, type Level } from '../api/courses';
import { describeCourseError } from '../api/courseMessages';
import type { ReviewedRevision } from '../api/courseReview';
import { CatalogueName, entryLabel } from '../catalogue';
import { setFlash, takeFlash, useStudio } from '../context';
import { courseState, courseTitle } from '../courses/courseLabels';
import { CourseDraftEditor } from '../courses/CourseDraftEditor';
import { ArchiveRequest, CourseStateNotice, CourseStatusView, NewerRevisionsNotice } from '../courses/CourseStatus';
import { t, tn } from '../i18n';
import { LANGUAGE_LABELS } from '../labels';
import { BASE, navigate } from '../router';
import { Link } from '../ui/Link';

type Loaded = {
  course: Course;
  revision: ReviewedRevision | null;
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
          setError(t('courses.page.notFound'));
          return;
        }
        const [subjects, levels, revision, lessons] = await Promise.all([
          api.subjects(),
          courses.levels(),
          course.latestRevision ? (courses.revision(course.latestRevision.id) as Promise<ReviewedRevision>) : Promise.resolve(null),
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
          <Link to={`${BASE}/courses`}>{t('shell.myCourses')}</Link>
        </p>
        <h1 tabIndex={-1}>{t('courses.course')}</h1>
        <div className="banner error" role="alert">
          <p>{error}</p>
          <button type="button" onClick={() => void reload()}>
            {t('shell.retry')}
          </button>
        </div>
      </main>
    );
  if (!loaded)
    return (
      <main className="page" id="main">
        <p role="status">{t('courses.page.loading')}</p>
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
        onDiscarded={() => toList(t('courses.page.draftDiscarded'))}
        onDeleted={() => toList(t('courses.page.deleted'))}
        onLeftDraft={() => void reload(t('courses.page.leftDraft'))}
        onSaved={(saved) => setSavedTitle(saved.title)}
        onSubmitted={(submitted) =>
          void reload(
            submitted.blocked
              ? t('courses.page.submittedBlocked', { number: submitted.number })
              : t('courses.page.submitted', { number: submitted.number }),
          )
        }
      />
    );
  } else {
    body = (
      <CourseStatusView
        course={course}
        revision={revision}
        onDraft={(created) => {
          setDropped({ steps: created.droppedSteps ?? [], revision: created });
          void reload(t('courses.page.draftCreated'));
        }}
        onDeleted={() => toList(t('courses.page.deleted'))}
      />
    );
  }
  const everSubmitted = revision !== null && (revision.status !== 'draft' || revision.number > 1);
  const showNotice = course.publishedRevisionId !== null || !!course.suspended;

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={`${BASE}/courses`}>{t('shell.myCourses')}</Link>
      </p>
      <h1 tabIndex={-1} lang={course.language} dir={dir}>
        {courseTitle(savedTitle ?? revision?.title ?? course.latestRevision?.title, course.id)}
      </h1>
      <p className="lesson-context">
        {t('courses.course')} · <CatalogueName label={entryLabel(subject, course.language)} /> · {LANGUAGE_LABELS[course.language]} · <span className={`state state-${state.tone}`}>{state.label}</span>
        {course.publishedRevisionId !== null && state.tone !== 'published' && <> · {t('courses.page.earlierPublished')}</>}
      </p>
      {notice && (
        <p className="banner success" role="status">
          {notice}
        </p>
      )}
      <CourseStateNotice course={course} />
      {showNotice && !course.archived && (
        <NewerRevisionsNotice
          key={`notice-${generation}`}
          course={course}
          draftOpen={revision?.status === 'draft'}
          onAmendment={(created) => void reload(t('courses.page.amendmentCreated', { number: created.number }))}
        />
      )}
      {body}
      {everSubmitted && revision?.status !== 'draft' && <ArchiveRequest key={`archive-${generation}`} course={course} />}
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
        {tn('courses.dropped.heading', dropped.length)}
      </h2>
      <p>{t('courses.dropped.text')}</p>
      <ul>
        {dropped.map((s) => {
          const module = moduleTitle(s.moduleKey);
          return (
            <li key={`${s.moduleKey}-${s.position}`}>
              {module
                ? t('courses.dropped.itemInModule', { title: s.title, lesson: s.lessonId, module, position: s.position })
                : t('courses.dropped.item', { title: s.title, lesson: s.lessonId, position: s.position })}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
