// New course: subject and language, fixed for the course's life. The server
// creates the course with an empty first draft (POST /api/courses), which
// opens in the course editor; an incomplete draft can be saved.
import { useEffect, useId, useMemo, useState, type FormEvent } from 'react';
import type { Subject } from '../api/client';
import { courseApi } from '../api/courses';
import { describeCourseError } from '../api/courseMessages';
import { entryLabel, labelAttrs } from '../catalogue';
import { setFlash, useStudio } from '../context';
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
      setFlash('Cours créé avec un brouillon vide, enregistré sur le serveur.');
      navigate(`${BASE}/courses/${course.id}`, { replace: true });
    } catch (e) {
      setError(describeCourseError(e));
      setBusy(false);
    }
  };

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={`${BASE}/courses`}>Mes cours</Link>
      </p>
      <h1 tabIndex={-1}>Nouveau cours</h1>
      {error && (
        <div className="banner error" role="alert">
          <p>{error}</p>
          {subjects === null && (
            <button type="button" onClick={loadSubjects}>
              Réessayer
            </button>
          )}
        </div>
      )}
      {subjects === null && !error && <p role="status">Chargement des matières…</p>}
      {subjects && subjects.length === 0 && <p>Le catalogue ne contient aucune matière ouverte. Un administrateur doit en ajouter avant toute création de cours.</p>}
      {subjects && subjects.length > 0 && (
        <form className="panel new-lesson" onSubmit={(e) => void create(e)}>
          <div className="fields">
            <label htmlFor={subjectField}>Matière</label>
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
            <legend>Langue du cours</legend>
            {(['fr', 'ar'] as const).map((l) => (
              <label key={l} className="choice">
                <input type="radio" name="course-language" value={l} checked={language === l} onChange={() => setLanguage(l)} /> {LANGUAGE_LABELS[l]}
                {l === 'ar' ? ' (droite à gauche)' : ''}
              </label>
            ))}
          </fieldset>
          <p className="hint">La matière et la langue ne pourront plus être changées : seules les leçons de cette matière et de cette langue pourront être ajoutées. Le cours est créé sur le serveur dès maintenant, avec un brouillon vide que vous compléterez.</p>
          <button type="submit" className="primary" disabled={busy || expired}>
            {busy ? 'Création…' : 'Créer le cours'}
          </button>
        </form>
      )}
    </main>
  );
}
