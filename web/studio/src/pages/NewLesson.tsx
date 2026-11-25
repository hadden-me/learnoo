// New lesson: subject and chapter from the catalogue, language, and an
// optional source lesson for a translation. The lesson is created on the
// server by its first save (POST /api/lessons), since a lesson always has a
// valid first draft; until then only the recovery buffer protects the text.
import { useEffect, useId, useState, type FormEvent } from 'react';
import type { Chapter, OwnedLesson, Revision, Subject } from '../api/client';
import { describeError } from '../api/messages';
import { setFlash, useStudio } from '../context';
import { DraftEditor } from '../editor/DraftEditor';
import { emptyData } from '../editor/convert';
import { LANGUAGE_LABELS, formatDateTime } from '../labels';
import { findRecovery, removeRecovery, type NewLessonParams, type RecoveryEntry } from '../recovery';
import { BASE, navigate, replacePath } from '../router';
import { Link } from '../ui/Link';

export function NewLesson() {
  const { api, session } = useStudio();
  const ids = { subject: useId(), chapter: useId(), translation: useId() };
  const [subjects, setSubjects] = useState<Subject[] | null>(null);
  const [owned, setOwned] = useState<OwnedLesson[]>([]);
  const [subjectId, setSubjectId] = useState<number | null>(null);
  const [chapters, setChapters] = useState<Chapter[]>([]);
  const [chapterId, setChapterId] = useState<number | null>(null);
  const [language, setLanguage] = useState<'fr' | 'ar'>('fr');
  const [translationOf, setTranslationOf] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [started, setStarted] = useState<{ params: NewLessonParams; chapters: Chapter[]; chapterId: number | null; restore?: RecoveryEntry } | null>(null);
  const [pending, setPending] = useState<RecoveryEntry[]>(() => findRecovery(localStorage, session.user.id, null, null));

  useEffect(() => {
    Promise.all([api.subjects(), api.myLessons()]).then(
      ([s, mine]) => {
        setSubjects(s);
        setOwned(mine);
        if (s.length > 0) setSubjectId(s[0].id);
      },
      (e) => setError(describeError(e)),
    );
  }, [api]);

  const subject = subjects?.find((s) => s.id === subjectId) ?? null;
  useEffect(() => {
    setChapters([]);
    setChapterId(null);
    setTranslationOf(null);
    if (!subject) return;
    api.chapters(subject.code).then(
      (r) => setChapters(r.chapters),
      (e) => setError(describeError(e)),
    );
  }, [api, subject]);

  const start = (event: FormEvent) => {
    event.preventDefault();
    if (!subject) return;
    setStarted({ params: { subjectId: subject.id, subjectCode: subject.code, language, translationOf }, chapters, chapterId });
  };

  const resume = async (entry: RecoveryEntry) => {
    const params = entry.newLesson;
    if (!params) return;
    try {
      const r = await api.chapters(params.subjectCode);
      setStarted({ params, chapters: r.chapters, chapterId: entry.data.chapterId, restore: entry });
    } catch (e) {
      setError(describeError(e));
    }
  };

  const [createdId, setCreatedId] = useState<number | null>(null);
  const created = (revision: Revision) => {
    setCreatedId(revision.lessonId);
    replacePath(`${BASE}/lessons/${revision.lessonId}/edit`);
  };
  const reopen = () => {
    if (createdId !== null) navigate(`${BASE}/lessons/${createdId}/edit`, { replace: true, force: true });
  };

  if (started)
    return (
      <main className="page" id="main">
        <p className="breadcrumb">
          <Link to={`${BASE}/`}>Mes leçons</Link>
        </p>
        <h1 tabIndex={-1}>Nouvelle leçon</h1>
        {createdId !== null && (
          <p className="banner success" role="status">
            Leçon créée et enregistrée sur le serveur.
          </p>
        )}
        <DraftEditor
          target={{ kind: 'lesson', params: started.params }}
          language={started.params.language}
          subjectCode={started.params.subjectCode}
          chapters={started.chapters}
          initial={emptyData(started.chapterId)}
          restore={started.restore}
          revisionLabel={createdId === null ? 'Pas encore créée : le premier enregistrement crée la leçon' : 'Brouillon · révision 1'}
          lessonId={null}
          newLesson={started.params}
          onCreated={created}
          onSubmitted={reopen}
          onDiscarded={() => {
            setFlash('Brouillon supprimé.');
            navigate(`${BASE}/`, { force: true });
          }}
        />
      </main>
    );

  const translatable = owned.filter((l) => l.subjectId === subjectId);

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={`${BASE}/`}>Mes leçons</Link>
      </p>
      <h1 tabIndex={-1}>Nouvelle leçon</h1>
      {error && (
        <p className="banner error" role="alert">
          {error}
        </p>
      )}
      {pending.length > 0 && (
        <section className="recovery" aria-labelledby="pending-heading">
          <h2 id="pending-heading">Leçons non enregistrées sur cet appareil</h2>
          <p className="hint">Copies de secours locales de leçons commencées mais jamais enregistrées sur le serveur.</p>
          <ul>
            {pending.map((e) => (
              <li key={e.key}>
                <p>
                  « {e.data.title || 'sans titre'} » · {e.newLesson?.subjectCode} · {e.newLesson ? LANGUAGE_LABELS[e.newLesson.language] : ''} · copie du {formatDateTime(e.savedAt)}
                </p>
                <div className="actions">
                  <button type="button" onClick={() => void resume(e)} disabled={!e.newLesson}>
                    Reprendre
                  </button>
                  <button
                    type="button"
                    onClick={() => {
                      removeRecovery(localStorage, e.key);
                      setPending(findRecovery(localStorage, session.user.id, null, null));
                    }}
                  >
                    Supprimer cette copie
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </section>
      )}
      {subjects && subjects.length === 0 && <p>Le catalogue ne contient aucune matière. Un opérateur doit en ajouter (commande add-subject) avant toute création de leçon.</p>}
      {subjects && subjects.length > 0 && (
        <form className="panel new-lesson" onSubmit={start}>
          <div className="fields">
            <label htmlFor={ids.subject}>Matière</label>
            <select id={ids.subject} value={subjectId ?? ''} onChange={(e) => setSubjectId(Number(e.target.value))}>
              {subjects.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.code}
                </option>
              ))}
            </select>
            <label htmlFor={ids.chapter}>Chapitre (facultatif)</label>
            <select id={ids.chapter} value={chapterId ?? ''} onChange={(e) => setChapterId(e.target.value ? Number(e.target.value) : null)}>
              <option value="">Sans chapitre</option>
              {chapters.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.code}
                </option>
              ))}
            </select>
          </div>
          <fieldset className="choices">
            <legend>Langue de la leçon</legend>
            {(['fr', 'ar'] as const).map((l) => (
              <label key={l} className="choice">
                <input type="radio" name="language" value={l} checked={language === l} onChange={() => setLanguage(l)} /> {LANGUAGE_LABELS[l]}
                {l === 'ar' ? ' (droite à gauche)' : ''}
              </label>
            ))}
          </fieldset>
          <div className="fields">
            <label htmlFor={ids.translation}>Traduction d’une de vos leçons (facultatif)</label>
            <select id={ids.translation} value={translationOf ?? ''} onChange={(e) => setTranslationOf(e.target.value ? Number(e.target.value) : null)}>
              <option value="">Non, leçon indépendante</option>
              {translatable.map((l) => (
                <option key={l.id} value={l.id}>
                  {l.latestRevision?.title || `Leçon n° ${l.id}`} ({LANGUAGE_LABELS[l.language]})
                </option>
              ))}
            </select>
          </div>
          <p className="hint">La matière et la langue ne pourront plus être changées. La leçon est créée sur le serveur à son premier enregistrement.</p>
          <button type="submit" className="primary">
            Commencer la rédaction
          </button>
        </form>
      )}
    </main>
  );
}
