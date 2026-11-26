// "Mes leçons": the author's own lessons from GET /api/lessons/mine.
import { useEffect, useState } from 'react';
import type { OwnedLesson } from '../api/client';
import { describeError } from '../api/messages';
import { takeFlash, useStudio } from '../context';
import { CatalogueName } from '../catalogue';
import { LANGUAGE_LABELS, formatDateTime, lessonState, ownedLessonPlace } from '../labels';
import { t } from '../i18n';
import { BASE } from '../router';
import { Link } from '../ui/Link';

export function LessonList() {
  const { api } = useStudio();
  const [lessons, setLessons] = useState<OwnedLesson[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [flash] = useState(takeFlash);

  useEffect(() => {
    api.myLessons().then(setLessons, (e) => setError(describeError(e)));
  }, [api]);

  return (
    <main className="page" id="main">
      <div className="page-head">
        <h1 tabIndex={-1}>{t('shell.myLessons')}</h1>
        <Link to={`${BASE}/lessons/new`} className="button primary">
          {t('lessons.newLesson')}
        </Link>
      </div>
      {flash && (
        <p className="banner success" role="status">
          {flash}
        </p>
      )}
      {error && (
        <p className="banner error" role="alert">
          {error}
        </p>
      )}
      {!lessons && !error && <p role="status">{t('shell.loadingPage')}</p>}
      {lessons && lessons.length === 0 && <p>{t('lessons.list.empty')}</p>}
      {lessons && lessons.length > 0 && (
        <ul className="lesson-list" aria-label={t('lessons.list.label')}>
          {lessons.map((lesson) => {
            const latest = lesson.latestRevision;
            const state = lessonState(lesson);
            const place = ownedLessonPlace(lesson);
            return (
              <li key={lesson.id} className="lesson-card">
                <h2 lang={lesson.language} dir={lesson.language === 'ar' ? 'rtl' : 'ltr'}>
                  <Link to={`${BASE}/lessons/${lesson.id}/edit`}>{latest?.title || t('lessons.numbered', { id: lesson.id })}</Link>
                </h2>
                <p className="lesson-meta">
                  <span className={`state state-${state.tone}`}>{state.label}</span>
                  {lesson.hasFeedback && <span className="state state-feedback">{t('lessons.list.feedback')}</span>}
                  {lesson.publishedRevisionId !== null && state.tone !== 'published' && <span className="state state-published">{t('lessons.list.publishedVersion')}</span>}
                  {lesson.archived && <span className="state">{t('lessons.list.archived')}</span>}
                </p>
                <p className="lesson-context">
                  <CatalogueName label={place.subject} />
                  {place.chapter && (
                    <>
                      {' · '}
                      <CatalogueName label={place.chapter} />
                    </>
                  )}{' '}
                  · {LANGUAGE_LABELS[lesson.language]}
                  {latest && (
                    <>
                      {' '}
                      · {t('lessons.list.revision', { number: latest.number, version: latest.version, date: formatDateTime(latest.updatedAt) })}
                    </>
                  )}
                </p>
              </li>
            );
          })}
        </ul>
      )}
    </main>
  );
}
