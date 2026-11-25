// /studio/lessons/{id}/edit: the lesson's latest revision. A draft opens in
// the editor; a submitted revision is shown read-only with its review state,
// reviewer feedback and, when the workflow allows, a button that starts the
// successor draft (POST /api/lessons/{id}/drafts).
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { getSchema } from '@tiptap/core';
import type { Chapter, OwnedLesson, Revision } from '../api/client';
import { isApiError } from '../api/client';
import { describeError } from '../api/messages';
import { setFlash, takeFlash, useStudio } from '../context';
import { DraftEditor, Feedback } from '../editor/DraftEditor';
import { emptyData, revisionToData } from '../editor/convert';
import { LANGUAGE_LABELS, STATUS_LABELS, formatDateTime, lessonState } from '../labels';
import { disableVideos, loadKatex, typeset } from '../preview';
import { BASE, navigate } from '../router';
import { lessonExtensions } from '../schema/extensions';
import { Link } from '../ui/Link';
import { RecoveryList } from '../ui/RecoveryList';
import { findLessonRecovery } from '../recovery';

const schema = getSchema(lessonExtensions);

type Loaded = {
  lesson: OwnedLesson;
  chapters: Chapter[];
  revision: Revision | null;
  feedback: Revision | null;
};

export function EditorPage({ lessonId }: { lessonId: number }) {
  const { api } = useStudio();
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(takeFlash);
  const [generation, setGeneration] = useState(0);

  const reload = useCallback(
    async (message?: string) => {
      try {
        const lessons = await api.myLessons();
        const lesson = lessons.find((l) => l.id === lessonId);
        if (!lesson) {
          setError('Leçon introuvable : elle n’existe pas ou ne vous appartient pas.');
          return;
        }
        const [chapters, revision, feedback] = await Promise.all([
          api.chapters(lesson.subjectCode).then((c) => c.chapters),
          lesson.latestRevision ? api.revision(lesson.latestRevision.id) : Promise.resolve(null),
          lesson.hasFeedback && lesson.feedbackRevisionId !== null ? api.revision(lesson.feedbackRevisionId) : Promise.resolve(null),
        ]);
        setLoaded({ lesson, chapters, revision, feedback });
        setGeneration((g) => g + 1);
        setError(null);
        if (message) setNotice(message);
      } catch (e) {
        setError(describeError(e));
      }
    },
    [api, lessonId],
  );

  useEffect(() => {
    void reload();
  }, [reload]);

  if (error)
    return (
      <main className="page" id="main">
        <h1 tabIndex={-1}>Leçon</h1>
        <p className="banner error" role="alert">
          {error}
        </p>
        <p>
          <Link to={`${BASE}/`}>Revenir à mes leçons</Link>
        </p>
      </main>
    );
  if (!loaded)
    return (
      <main className="page" id="main">
        <p role="status">Chargement de la leçon…</p>
      </main>
    );

  const { lesson, chapters, revision, feedback } = loaded;
  const title = revision?.title || lesson.latestRevision?.title || `Leçon n° ${lesson.id}`;
  const dir = lesson.language === 'ar' ? 'rtl' : 'ltr';
  const feedbackBlock = feedback ? <Feedback revision={feedback} /> : null;

  let body;
  if (revision && revision.status === 'draft') {
    let initial;
    try {
      initial = revisionToData(schema, revision);
    } catch (e) {
      body = (
        <p className="banner error" role="alert">
          Ce brouillon ne peut pas être ouvert dans l’éditeur : {(e as Error).message}
        </p>
      );
    }
    if (initial)
      body = (
        <DraftEditor
          key={`${revision.id}-${generation}`}
          target={{ kind: 'revision', revisionId: revision.id, version: revision.version }}
          language={lesson.language}
          subjectCode={lesson.subjectCode}
          chapters={chapters}
          initial={initial}
          revisionLabel={`Brouillon · révision ${revision.number}`}
          lessonId={lesson.id}
          revisionNumber={revision.number}
          feedback={feedbackBlock}
          onSubmitted={() => void reload('Cette révision a quitté l’état de brouillon : elle est affichée en lecture seule.')}
          onDiscarded={() => {
            setFlash('Brouillon supprimé.');
            navigate(`${BASE}/`, { force: true });
          }}
        />
      );
  } else if (!revision) {
    body = (
      <>
        <p className="banner" role="note">
          Cette leçon n’a pas de version en cours (son brouillon a été supprimé). Rédigez un nouveau brouillon ci-dessous.
        </p>
        <DraftEditor
          key={`empty-${generation}`}
          target={{ kind: 'draft', lessonId: lesson.id }}
          language={lesson.language}
          subjectCode={lesson.subjectCode}
          chapters={chapters}
          initial={emptyData()}
          revisionLabel="Nouveau brouillon"
          lessonId={lesson.id}
          feedback={feedbackBlock}
          // The editor stays mounted (no reload), so text typed during this first save is kept.
          onCreated={() => setNotice('Brouillon enregistré.')}
          onSubmitted={() => void reload('Révision soumise pour relecture. Elle ne peut plus être modifiée.')}
          onDiscarded={() => {
            setFlash('Brouillon supprimé.');
            navigate(`${BASE}/`, { force: true });
          }}
        />
      </>
    );
  } else {
    body = <SubmittedRevision lesson={lesson} revision={revision} feedback={feedbackBlock} onDraft={() => void reload('Nouvelle version créée : vous pouvez la modifier.')} />;
  }

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={`${BASE}/`}>Mes leçons</Link>
      </p>
      <h1 tabIndex={-1} lang={lesson.language} dir={dir}>
        {title}
      </h1>
      <p className="lesson-context">
        {lesson.subjectCode} · {LANGUAGE_LABELS[lesson.language]} · <span className={`state state-${lessonState(lesson).tone}`}>{lessonState(lesson).label}</span>
        {lesson.publishedRevisionId !== null && lessonState(lesson).tone !== 'published' && <> · une version antérieure est publiée</>}
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

/** A revision that left draft: read-only, with its review state and the way forward. */
function SubmittedRevision({ lesson, revision, feedback, onDraft }: { lesson: OwnedLesson; revision: Revision; feedback: ReactNode; onDraft: () => void }) {
  const { api, session } = useStudio();
  const [copies, setCopies] = useState(() => findLessonRecovery(localStorage, session.user.id, lesson.id));
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const previewRef = useRef<HTMLDivElement>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api.preview(revision.content).then(
      async (html) => {
        const root = previewRef.current;
        if (cancelled || !root) return;
        root.innerHTML = html;
        disableVideos(root);
        typeset(root, await loadKatex());
      },
      (e) => !cancelled && setPreviewError(describeError(e)),
    );
    return () => {
      cancelled = true;
    };
  }, [api, revision]);

  const published = lesson.publishedRevisionId === revision.id;
  const explanation =
    revision.status === 'in_review'
      ? 'Cette révision est en relecture. Elle ne peut plus être modifiée ; un relecteur de la matière va l’approuver ou demander des modifications.'
      : revision.status === 'changes_requested'
        ? 'Le relecteur a demandé des modifications. La révision est conservée telle quelle : vos corrections seront une nouvelle version, à soumettre de nouveau.'
        : published
          ? 'Cette révision est publiée. Pour la modifier, créez une nouvelle version, qui sera relue avant publication.'
          : 'Cette révision est approuvée et attend sa publication. Une nouvelle version ne pourra être créée qu’après sa publication ou son retrait.';
  const canStart = revision.status === 'changes_requested' || (revision.status === 'approved' && published);

  const startDraft = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.startDraft(lesson.id, { content: revision.content, prerequisites: revision.prerequisites, chapterId: revision.chapterId, mediaIds: revision.mediaIds });
      onDraft();
    } catch (e) {
      setError(
        isApiError(e, 'active_candidate_exists')
          ? 'Impossible de créer une nouvelle version : la leçon a déjà une version en cours (brouillon, révision en relecture, ou révision approuvée en attente de publication). Elle doit d’abord être publiée ou retirée par un administrateur.'
          : describeError(e),
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="editor-page">
      <section className="status-card" aria-labelledby="status-heading">
        <h2 id="status-heading">
          Révision {revision.number} · {published ? 'Publiée' : STATUS_LABELS[revision.status]}
        </h2>
        <p>{explanation}</p>
        {revision.submittedAt && <p className="hint">Soumise le {formatDateTime(revision.submittedAt)}.</p>}
        {revision.review?.action === 'approve' && <p className="hint">Approuvée le {formatDateTime(revision.review.decidedAt)}.</p>}
        {revision.review?.action === 'approve' && revision.review.comment && <blockquote dir="auto">{revision.review.comment}</blockquote>}
        {canStart && (
          <button type="button" className="primary" onClick={() => void startDraft()} disabled={busy}>
            {revision.status === 'changes_requested' ? 'Créer la version corrigée' : 'Créer une nouvelle version'}
          </button>
        )}
        {error && (
          <p className="banner error" role="alert">
            {error}
          </p>
        )}
      </section>
      <RecoveryList
        entries={copies}
        currentRevisionId={revision.id}
        language={lesson.language}
        onChanged={() => setCopies(findLessonRecovery(localStorage, session.user.id, lesson.id))}
        note={
          canStart
            ? 'Cette révision ne peut plus être modifiée : créez la nouvelle version ci-dessus pour restaurer une copie, ou copiez-la.'
            : 'Cette révision ne peut plus être modifiée : copiez ces modifications pour les reprendre dans la prochaine version de la leçon.'
        }
      />
      {feedback}
      <section className="panel" aria-labelledby="readonly-heading">
        <h2 id="readonly-heading">Révision {revision.number} — lecture seule</h2>
        {revision.mediaIds.length > 0 && (
          <ul className="media-list" aria-label="Images jointes">
            {revision.mediaIds.map((id) => (
              <li key={id}>
                <img src={`/api/media/${id}`} alt={`Image jointe n° ${id}`} />
                <span>Image n° {id}</span>
              </li>
            ))}
          </ul>
        )}
        {previewError && (
          <p className="banner error" role="alert">
            Aperçu impossible. {previewError}
          </p>
        )}
        <div ref={previewRef} className="lesson-preview" data-testid="revision-preview" />
      </section>
    </div>
  );
}
