// New course: subject and language, fixed for the course's life. The server
// creates the course with an empty first draft (POST /api/courses), which
// opens in the course editor; an incomplete draft can be saved.
import { useEffect, useId, useMemo, useState, type FormEvent } from 'react';
import type { Subject } from '../api/client';
import { courseApi } from '../api/courses';
import { describeCourseError } from '../api/courseMessages';
import { entryLabel, labelAttrs } from '../catalogue';
import { setFlash, useStudio } from '../context';
import { t } from '../i18n';
import { LANGUAGE_LABELS } from '../labels';
import { BASE, navigate } from '../router';
import { Link } from '../ui/Link';

export function NewCourse() {
  const { api, expired } = useStudio();
  const courses = useMemo(() => courseApi(api), [api]);
  const subjectField = useId();
  const [subjects, setSubjects] = useState<Subject[] | null>(null);
  const [subjectId, setSubjectId] = useState<number | null>(null);
  const [language, setLanguage] = useState<'fr' | 'ar'>('fr');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const loadSubjects = () => {
    setError(null);
    api.subjects().then(
      (s) => {
        const open = s.filter((x) => !x.retiredAt);
        setSubjects(open);
        if (open.length > 0) setSubjectId(open[0].id);
      },
      (e) => setError(describeCourseError(e)),
    );
  };
  useEffect(loadSubjects, [api]);

  const create = async (event: FormEvent) => {
    event.preventDefault();
    if (subjectId === null || busy || expired) return;
    setBusy(true);
    setError(null);
    try {
      const { course } = await courses.createCourse(subjectId, language);
      setFlash(t('courses.new.created'));
      navigate(`${BASE}/courses/${course.id}`, { replace: true });
    } catch (e) {
      setError(describeCourseError(e));
      setBusy(false);
    }
  };

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={`${BASE}/courses`}>{t('shell.myCourses')}</Link>
      </p>
      <h1 tabIndex={-1}>{t('courses.list.new')}</h1>
      {error && (
        <div className="banner error" role="alert">
          <p>{error}</p>
          {subjects === null && (
            <button type="button" onClick={loadSubjects}>
              {t('shell.retry')}
            </button>
          )}
        </div>
      )}
      {subjects === null && !error && <p role="status">{t('courses.new.loadingSubjects')}</p>}
      {subjects && subjects.length === 0 && <p>{t('courses.new.noSubjects')}</p>}
      {subjects && subjects.length > 0 && (
        <form className="panel new-lesson" onSubmit={(e) => void create(e)}>
          <div className="fields">
            <label htmlFor={subjectField}>{t('courses.new.subject')}</label>
            <select id={subjectField} value={subjectId ?? ''} onChange={(e) => setSubjectId(Number(e.target.value))}>
              {subjects.map((s) => {
                const label = entryLabel(s, language);
                return (
                  <option key={s.id} value={s.id} {...labelAttrs(label)}>
                    {label.text}
                  </option>
                );
              })}
            </select>
          </div>
          <fieldset className="choices">
            <legend>{t('courses.new.language')}</legend>
            {(['fr', 'ar'] as const).map((l) => (
              <label key={l} className="choice">
                <input type="radio" name="course-language" value={l} checked={language === l} onChange={() => setLanguage(l)} /> {LANGUAGE_LABELS[l]}
                {l === 'ar' ? ` ${t('courses.new.rtl')}` : ''}
              </label>
            ))}
          </fieldset>
          <p className="hint">{t('courses.new.hint')}</p>
          <button type="submit" className="primary" disabled={busy || expired}>
            {busy ? t('courses.new.creating') : t('courses.new.create')}
          </button>
        </form>
      )}
    </main>
  );
}
