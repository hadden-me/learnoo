// Administrator space (the plan's minimal emergency actions): find a lesson,
// then on its publication page withdraw a revision, unpublish the lesson
// in an emergency, or restore a revision published before. Restoration is
// also open to publishers. Every action names the published revision this
// page loaded (expectedPublishedRevisionId); if anyone changed it since,
// the server refuses with stale_pointer and nothing changes.
import { useRef, useState, type FormEvent } from 'react';
import { isApiError } from '../api/client';
import type { LessonPublication, LessonRevisionState } from '../api/editorial';
import { describeEditorialError } from '../api/editorialMessages';
import { useStudio } from '../context';
import { lessonRefFromInput, paths } from '../editorialRoutes';
import { LANGUAGE_LABELS, formatDateTime, formatTime } from '../labels';
import { navigate } from '../router';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { ErrorPage, LoadingPage, NoteField, Quote, dirOf, useEditorial, useLoad } from './common';
import { canUnpublish, describeEvent, describeSubmission, revisionActions, revisionStateLabel } from './policy';

export function AdminLessonsPage() {
  const ed = useEditorial();
  const [lessons] = useLoad(() => ed.adminLessons(), [ed]);
  const [ref, setRef] = useState('');
  const [refError, setRefError] = useState<string | null>(null);
  const input = useRef<HTMLInputElement>(null);

  const find = (e: FormEvent) => {
    e.preventDefault();
    const parsed = lessonRefFromInput(ref);
    if (!parsed) {
      setRefError('Saisissez l’identifiant public de la leçon (il figure dans son adresse : /learn/lessons/…), l’adresse elle-même ou le numéro de la leçon.');
      input.current?.focus();
      return;
    }
    navigate(paths.publication(parsed));
  };

  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>Administration</h1>
      <p className="hint">Actions d’urgence sur les leçons : retrait d’une révision, dépublication, restauration d’une version publiée auparavant. Chaque action est enregistrée avec son motif.</p>
      <form className="find-lesson" onSubmit={find} noValidate>
        <div className="fields">
          <label htmlFor="lesson-ref">Retrouver une leçon</label>
          <p className="hint" id="lesson-ref-hint">
            Identifiant public, adresse de la leçon dans le lecteur, ou numéro de leçon.
          </p>
          {refError && (
            <p className="field-error" id="lesson-ref-error">
              {refError}
            </p>
          )}
          <input
            id="lesson-ref"
            ref={input}
            value={ref}
            dir="ltr"
            autoComplete="off"
            spellCheck={false}
            aria-invalid={refError ? true : undefined}
            aria-describedby={`lesson-ref-hint${refError ? ' lesson-ref-error' : ''}`}
            onChange={(e) => setRef(e.target.value)}
          />
        </div>
        <button type="submit">Ouvrir</button>
      </form>
      <h2>Leçons approuvées ou publiées</h2>
      {lessons.state === 'loading' && <p role="status">Chargement des leçons…</p>}
      {lessons.state === 'error' && (
        <p className="banner error" role="alert">
          {lessons.message}
        </p>
      )}
      {lessons.state === 'ready' && lessons.data.length === 0 && <p>Aucune leçon n’a encore de révision approuvée.</p>}
      {lessons.state === 'ready' && lessons.data.length > 0 && (
        <ul className="lesson-list" aria-label="Leçons approuvées ou publiées">
          {lessons.data.map((l) => (
            <li key={l.lessonId} className="lesson-card">
              <h3 lang={l.language} dir={dirOf(l.language)}>
                <Link to={paths.publication(l.publicId)}>{l.title}</Link>
              </h3>
              <p className="lesson-meta">
                {l.publishedRevisionId !== null ? <span className="state state-published">Publiée</span> : <span className="state">Non publiée</span>}
                {l.pendingRevisionId !== null && <span className="state state-approved">Révision approuvée en attente</span>}
                {l.archived && <span className="state">Archivée</span>}
              </p>
              <p className="lesson-context">
                {l.subjectCode} · {LANGUAGE_LABELS[l.language]} · dernier changement le {formatDateTime(l.lastChange)}
              </p>
            </li>
          ))}
        </ul>
      )}
    </main>
  );
}

type Pending =
  | { kind: 'withdraw'; revision: LessonRevisionState }
  | { kind: 'restore'; revision: LessonRevisionState }
  | { kind: 'unpublish' };

export function LessonPublicationPage({ lessonRef }: { lessonRef: string }) {
  const ed = useEditorial();
  const { spaces } = useStudio();
  const [loadedAt, setLoadedAt] = useState(() => new Date());
  const [loaded, reload] = useLoad(async () => {
    const lp = await ed.lessonPublication(lessonRef);
    setLoadedAt(new Date());
    return lp;
  }, [ed, lessonRef]);
  // Which inline reason form is open: a revision ID (withdrawal) or 'unpublish'.
  const [form, setForm] = useState<number | 'unpublish' | null>(null);
  const [reason, setReason] = useState('');
  const [reasonError, setReasonError] = useState<string | null>(null);
  const [pending, setPending] = useState<Pending | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ message: string; stale: boolean } | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const reasonRef = useRef<HTMLTextAreaElement>(null);
  const doneRef = useRef<HTMLParagraphElement>(null);

  if (loaded.state === 'loading') return <LoadingPage label="Chargement de la leçon…" />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title="Leçon introuvable"
        message={loaded.notFound ? 'Aucune leçon ne correspond à cet identifiant.' : loaded.message}
        back={spaces.administrator ? { to: paths.adminLessons, label: 'Revenir à l’administration' } : { to: paths.publishQueue, label: 'Revenir à la publication' }}
      />
    );
  const lesson: LessonPublication = loaded.data;
  const byId = new Map(lesson.revisions.map((r) => [r.id, r]));
  const numberOf = (id: number | null) => (id === null ? '—' : byId.has(id) ? String(byId.get(id)!.number) : `n° ${id}`);
  const current = lesson.publishedRevisionId !== null ? byId.get(lesson.publishedRevisionId) : undefined;
  const title = current?.title ?? lesson.revisions[0]?.title ?? `Leçon n° ${lesson.lessonId}`;
  const expectedLabel = lesson.publishedRevisionId === null ? 'aucune version publiée' : `la révision ${numberOf(lesson.publishedRevisionId)}`;

  const openForm = (which: number | 'unpublish') => {
    setForm(which);
    setReason('');
    setReasonError(null);
    setError(null);
    requestAnimationFrame(() => reasonRef.current?.focus());
  };

  const askWithReason = (next: Pending) => {
    if (!reason.trim()) {
      setReasonError('Indiquez un motif : il est obligatoire et sera conservé dans l’historique.');
      reasonRef.current?.focus();
      return;
    }
    setReasonError(null);
    setPending(next);
  };

  const run = async (action: Pending) => {
    setBusy(true);
    const expected = lesson.publishedRevisionId;
    try {
      if (action.kind === 'withdraw') {
        const w = await ed.withdraw(action.revision.id, reason, expected);
        setDone(`Révision ${action.revision.number} retirée définitivement.${w.unpublication ? ' C’était la version publiée : la leçon n’est plus lue par les élèves.' : ''}`);
      } else if (action.kind === 'unpublish') {
        await ed.unpublish(lesson.lessonId, reason, expected!);
        setDone('Leçon dépubliée : les élèves ne peuvent plus la lire. La révision reste approuvée et peut être restaurée.');
      } else {
        await ed.restore(action.revision.id, expected);
        setDone(`Révision ${action.revision.number} restaurée : c’est maintenant la version lue par les élèves.`);
      }
      setPending(null);
      setForm(null);
      setReason('');
      setError(null);
      await reload();
      // The button that was pressed may be gone (withdrawn revision, closed form): move focus to the outcome.
      requestAnimationFrame(() => doneRef.current?.focus());
    } catch (e) {
      setPending(null);
      setDone(null);
      setError({ message: describeEditorialError(e), stale: isApiError(e, 'stale_pointer') });
      if (isApiError(e, 'reason_required') || isApiError(e, 'invalid_reason')) reasonRef.current?.focus();
    } finally {
      setBusy(false);
    }
  };

  const reasonForm = (which: number | 'unpublish', submitLabel: string, onSubmit: () => void) =>
    form === which && (
      <form
        className="reason-form"
        onSubmit={(e) => {
          e.preventDefault();
          onSubmit();
        }}
        noValidate
      >
        <NoteField
          id={`reason-${which}`}
          label="Motif (obligatoire)"
          hint="Conservé dans l’historique de la leçon, 2 000 caractères au maximum."
          value={reason}
          onChange={(v) => {
            setReason(v);
            if (reasonError && v.trim()) setReasonError(null);
          }}
          error={reasonError}
          fieldRef={reasonRef}
        />
        <div className="actions">
          <button type="submit" className="danger">
            {submitLabel}
          </button>
          <button type="button" onClick={() => setForm(null)}>
            Annuler
          </button>
        </div>
      </form>
    );

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        {spaces.administrator ? <Link to={paths.adminLessons}>Administration</Link> : <Link to={paths.publishQueue}>Publication</Link>}
      </p>
      <h1 tabIndex={-1} lang={lesson.language} dir={dirOf(lesson.language)}>
        {title}
      </h1>
      <p className="lesson-context">
        {lesson.subjectCode} · {LANGUAGE_LABELS[lesson.language]} · leçon n° {lesson.lessonId} · identifiant public <code>{lesson.publicId}</code>
        {lesson.archived && ' · archivée'}
      </p>
      <section className="status-card safeguard" aria-labelledby="pointer-heading">
        <h2 id="pointer-heading">{current ? `Version publiée : révision ${current.number}` : 'Aucune version publiée'}</h2>
        <p>
          État chargé à {formatTime(loadedAt)}. Les actions ci-dessous supposent que la version publiée est {expectedLabel}. Si quelqu’un la change entre-temps, le serveur refuse l’action et
          rien n’est modifié.
        </p>
        <div className="actions">
          {current && (
            <a className="button" href={paths.readerLesson(lesson.publicId)}>
              Voir la leçon publiée
            </a>
          )}
          <button type="button" onClick={() => void reload().then(() => setError(null))}>
            Recharger l’état actuel
          </button>
          {canUnpublish(lesson, spaces) && form !== 'unpublish' && (
            <button type="button" className="danger" onClick={() => openForm('unpublish')}>
              Dépublier en urgence…
            </button>
          )}
        </div>
        {reasonForm('unpublish', 'Dépublier…', () => askWithReason({ kind: 'unpublish' }))}
      </section>
      {done && (
        <p className="banner success" role="status" tabIndex={-1} ref={doneRef}>
          {done}
        </p>
      )}
      {error && (
        <div className="banner error" role="alert">
          <p>{error.message}</p>
          {error.stale && (
            <button type="button" onClick={() => void reload().then(() => setError(null))}>
              Recharger l’état actuel
            </button>
          )}
        </div>
      )}
      <h2>{lesson.view === 'administrator' ? 'Révisions' : 'Révisions approuvées'}</h2>
      {lesson.revisions.length === 0 && <p>{lesson.view === 'administrator' ? 'Aucune révision soumise.' : 'Aucune révision approuvée.'}</p>}
      <ul className="lesson-list" aria-label="Révisions soumises de la leçon">
        {lesson.revisions.map((r) => {
          const state = revisionStateLabel(lesson, r);
          const can = revisionActions(lesson, r, spaces);
          return (
            <li key={r.id} className="lesson-card" aria-label={`Révision ${r.number}`}>
              <h3 lang={lesson.language} dir={dirOf(lesson.language)}>
                Révision {r.number} · {r.title}
              </h3>
              <p className="lesson-meta">
                <span className={`state state-${state.tone}`}>{state.label}</span>
              </p>
              <p className="lesson-context">
                {r.authorName ? `Par ${r.authorName}` : 'Approuvée'}
                {!r.submission && r.submittedAt && `, soumise le ${formatDateTime(r.submittedAt)}`}
                {r.approverName && r.approvedAt && ` · approuvée par ${r.approverName} le ${formatDateTime(r.approvedAt)}`}
                {!r.approverName && r.approvedAt && ` le ${formatDateTime(r.approvedAt)}`}
              </p>
              {r.submission && <p className="lesson-context">{describeSubmission(r, formatDateTime)}</p>}
              {r.withdrawn && !r.withdrawal && <p className="withdrawal">Retirée : elle ne peut plus être publiée ni restaurée.</p>}
              {r.withdrawal && (
                <div className="withdrawal">
                  <p>
                    Retirée par {r.withdrawal.withdrawnByName} le {formatDateTime(r.withdrawal.withdrawnAt)}. Elle ne peut plus être publiée ni restaurée. Motif :
                  </p>
                  <Quote>{r.withdrawal.reason}</Quote>
                </div>
              )}
              {(can.restore || (can.withdraw && form !== r.id)) && (
                <div className="actions">
                  {can.restore && (
                    <button type="button" onClick={() => setPending({ kind: 'restore', revision: r })}>
                      Restaurer cette révision…
                    </button>
                  )}
                  {can.withdraw && form !== r.id && (
                    <button type="button" className="danger" onClick={() => openForm(r.id)}>
                      Retirer cette révision…
                    </button>
                  )}
                </div>
              )}
              {reasonForm(r.id, 'Retirer…', () => askWithReason({ kind: 'withdraw', revision: r }))}
            </li>
          );
        })}
      </ul>
      {lesson.view === 'publisher' && (
        <p className="hint">
          Vue éditeur : seules les révisions approuvées sont listées. Les auteurs, les motifs de retrait et l’historique de publication sont réservés aux administrateurs.
        </p>
      )}
      {lesson.view === 'administrator' && <h2>Historique de publication</h2>}
      {lesson.view === 'publisher' ? null : lesson.history.length === 0 ? (
        <p>Cette leçon n’a jamais été publiée.</p>
      ) : (
        <ol className="history" aria-label="Historique de publication, le plus récent d’abord">
          {lesson.history.map((e, i) => (
            <li key={i}>
              <p>
                <strong>{describeEvent(e, numberOf)}</strong>
              </p>
              <p className="hint">
                Par {e.actorName}, le {formatDateTime(e.occurredAt)}
              </p>
              {e.reason && <Quote>{e.reason}</Quote>}
            </li>
          ))}
        </ol>
      )}
      {pending && (
        <Dialog
          title={
            pending.kind === 'withdraw'
              ? `Retirer la révision ${pending.revision.number} ?`
              : pending.kind === 'restore'
                ? `Restaurer la révision ${pending.revision.number} ?`
                : 'Dépublier la leçon ?'
          }
          onCancel={() => setPending(null)}
          cancelLabel="Ne rien changer"
          actions={[
            {
              label: pending.kind === 'withdraw' ? 'Retirer définitivement' : pending.kind === 'restore' ? 'Restaurer' : 'Dépublier',
              kind: pending.kind === 'restore' ? 'primary' : 'danger',
              disabled: busy,
              onClick: () => void run(pending),
            },
          ]}
        >
          {pending.kind === 'withdraw' && (
            <p>
              Le retrait est définitif : cette révision ne pourra plus jamais être publiée ni restaurée.
              {lesson.publishedRevisionId === pending.revision.id ? ' C’est la version publiée : la leçon sera aussi dépubliée.' : ''}
            </p>
          )}
          {pending.kind === 'unpublish' && <p>Les élèves ne pourront plus lire la leçon. La révision reste approuvée et pourra être restaurée.</p>}
          {pending.kind === 'restore' && <p>Cette révision redeviendra immédiatement la version lue par les élèves.</p>}
          <p>Version publiée attendue : {expectedLabel}.</p>
          {pending.kind !== 'restore' && (
            <>
              <p>Motif :</p>
              <Quote>{reason.trim()}</Quote>
            </>
          )}
        </Dialog>
      )}
    </main>
  );
}
