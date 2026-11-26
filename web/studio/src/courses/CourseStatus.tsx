// The author's views of a course outside its draft (LMS-B06): the state of
// its latest revision (in review, possibly blocked for want of an eligible
// reviewer; changes requested with the reviewer's feedback; approved;
// published; withdrawn), the course's own state (suspended, archived), the
// newer-lesson-revision notice with amendment creation, and the archive
// request. The server decides everything; these explain its answers.
import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { isApiError } from '../api/client';
import { courseApi, type Course, type CourseRevision } from '../api/courses';
import { courseReviewApi, type NewerRevisions, type ReviewedRevision } from '../api/courseReview';
import { describeCourseWorkflowError } from '../api/courseReviewMessages';
import { useStudio } from '../context';
import { NoteField, Quote } from '../editorial/common';
import { formatDateTime } from '../labels';
import { Dialog } from '../ui/Dialog';
import { COURSE_STATUS_LABELS } from './courseLabels';
import { describeBlocked } from './workflow';

/** Whether the course has no open candidate, so the owner may start a new draft (the server decides). */
export function canStartDraft(course: Course, revision: ReviewedRevision | null): boolean {
  if (course.archived) return false;
  if (!revision || revision.withdrawnAt) return true;
  if (revision.status === 'changes_requested') return true;
  return revision.status === 'approved' && (course.publishedRevisionId === revision.id || !!course.suspended);
}

/** Archive and suspension, which decide whether students can read the course whatever revision is in progress. */
export function CourseStateNotice({ course }: { course: Course }) {
  if (course.archived)
    return (
      <section className="status-card warning" aria-labelledby="course-state-heading">
        <h2 id="course-state-heading">Cours archivé</h2>
        <p>Un administrateur a archivé ce cours : il n’apparaît plus dans le catalogue et les élèves ne peuvent plus le lire. Son historique est conservé. Seul un administrateur peut le désarchiver ; il ne sera pas republié automatiquement.</p>
      </section>
    );
  if (course.suspended)
    return (
      <section className="status-card warning" aria-labelledby="course-state-heading">
        <h2 id="course-state-heading">Cours suspendu</h2>
        <p>
          Un administrateur a suspendu ce cours : il n’apparaît plus dans le catalogue et les élèves ne peuvent plus le lire. Pour le rendre de nouveau lisible, une version relue doit être publiée (un
          amendement si une leçon a une révision plus récente, sinon une nouvelle version), ou un administrateur le restaure une fois toutes ses leçons de nouveau publiables.
        </p>
      </section>
    );
  return null;
}

function Feedback({ revision }: { revision: ReviewedRevision }) {
  const r = revision.review;
  if (!r) return null;
  return (
    <section className="feedback" aria-labelledby="feedback-heading">
      <h3 id="feedback-heading">{r.action === 'request_changes' ? 'Modifications demandées par le relecteur' : 'Commentaire du relecteur'}</h3>
      <p>
        {r.reviewerName}, le {formatDateTime(r.decidedAt)}
      </p>
      {r.comment ? <Quote>{r.comment}</Quote> : <p>Sans commentaire.</p>}
    </section>
  );
}

export function CourseStatusView({
  course,
  revision,
  onDraft,
  onDeleted,
}: {
  course: Course;
  revision: ReviewedRevision | null;
  onDraft: (created: CourseRevision) => void;
  onDeleted: () => void;
}) {
  const { api, expired } = useStudio();
  const courses = useMemo(() => courseApi(api), [api]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirm, setConfirm] = useState(false);
  const errorRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (error) errorRef.current?.focus();
  }, [error]);

  const start = async () => {
    setBusy(true);
    setError(null);
    try {
      onDraft(await courses.startDraft(course.id));
    } catch (e) {
      setError(describeCourseWorkflowError(e));
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
      setError(describeCourseWorkflowError(e));
    }
  };

  const published = revision !== null && course.publishedRevisionId === revision.id;
  let heading: string;
  let body: React.ReactNode;
  if (!revision) {
    heading = 'Aucun brouillon en cours';
    body = <p>Le brouillon de ce cours a été supprimé. Créez un nouveau brouillon pour reprendre la rédaction, ou supprimez le cours s’il n’a jamais été soumis.</p>;
  } else if (revision.withdrawnAt) {
    heading = `Version ${revision.number} · retirée par un administrateur`;
    body = <p>Un administrateur a retiré cette version le {formatDateTime(revision.withdrawnAt)} : elle ne sera ni relue ni publiée. Vous pouvez rédiger une nouvelle version.</p>;
  } else if (revision.status === 'in_review') {
    const blocked = revision.blocked ? describeBlocked(revision.blocked) : null;
    heading = `Version ${revision.number} · En relecture`;
    body = (
      <>
        <p>
          {revision.kind === 'amendment' ? 'Cet amendement' : 'Cette version'} attend la décision d’un relecteur de la matière. Elle ne peut plus être modifiée.
          {revision.submittedAt && ` Soumise le ${formatDateTime(revision.submittedAt)}.`}
        </p>
        {blocked && (
          <div className="status-card warning" role="note" aria-labelledby="blocked-heading">
            <h3 id="blocked-heading">Relecture bloquée : aucun relecteur éligible</h3>
            <p>{blocked.summary}</p>
            {blocked.details.length > 0 && (
              <ul>
                {blocked.details.map((d) => (
                  <li key={d}>{d}</li>
                ))}
              </ul>
            )}
            <p>Personne ne peut approuver à la place d’un relecteur éligible : cette version ne peut pas être publiée tant que la situation n’a pas changé.</p>
          </div>
        )}
      </>
    );
  } else if (revision.status === 'changes_requested') {
    heading = `Version ${revision.number} · Modifications demandées`;
    body = <p>Le relecteur a renvoyé cette version. Créez une version corrigée : elle reprend celle-ci, que vous modifierez avant de la soumettre de nouveau.</p>;
  } else if (revision.status === 'approved' && published) {
    heading = `Version ${revision.number} · Publiée`;
    body = <p>Les élèves lisent cette version. Une nouvelle version ne remplacera celle-ci qu’après sa relecture et sa publication.</p>;
  } else if (revision.status === 'approved' && (course.suspended || course.archived)) {
    heading = `Version ${revision.number} · Approuvée, publiée auparavant`;
    body = <p>Cette version n’est plus lue par les élèves depuis {course.archived ? 'l’archivage' : 'la suspension'} du cours.</p>;
  } else if (revision.status === 'approved') {
    heading = `Version ${revision.number} · Approuvée, en attente de publication`;
    body = <p>Un relecteur a approuvé cette version. Un éditeur doit maintenant la publier ; jusque-là, les élèves lisent la version publiée précédente, s’il y en a une.</p>;
  } else {
    heading = `Version ${revision.number} · ${COURSE_STATUS_LABELS[revision.status]}`;
    body = <p>Cette version du cours a quitté l’état de brouillon : elle ne peut plus être modifiée.</p>;
  }

  return (
    <section className="status-card" aria-labelledby="course-status-heading">
      <h2 id="course-status-heading">{heading}</h2>
      {body}
      {revision && <Feedback revision={revision} />}
      <div className="actions">
        {canStartDraft(course, revision) && (
          <button type="button" className="primary" onClick={() => void start()} disabled={busy || expired}>
            {!revision ? 'Créer un brouillon' : revision.status === 'changes_requested' ? 'Créer la version corrigée' : 'Créer une nouvelle version'}
          </button>
        )}
        {!revision && (
          <button type="button" className="danger" onClick={() => setConfirm(true)} disabled={busy || expired}>
            Supprimer le cours
          </button>
        )}
      </div>
      {error && (
        <p className="banner error" role="alert" tabIndex={-1} ref={errorRef}>
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

/**
 * "A newer eligible revision is available": steps of the published course
 * whose lesson has a newer publishable revision. The owner chooses the
 * replacements and creates an amendment in one action; only one candidate
 * per course exists, so an open draft is reported (draft_exists).
 */
export function NewerRevisionsNotice({ course, draftOpen, onAmendment }: { course: Course; draftOpen: boolean; onAmendment: (created: ReviewedRevision) => void }) {
  const { api, expired } = useStudio();
  const review = useMemo(() => courseReviewApi(api), [api]);
  const [notice, setNotice] = useState<NewerRevisions | null>(null);
  const [loadError, setLoadError] = useState(false);
  const [chosen, setChosen] = useState<Set<number>>(new Set());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ text: string; draftExists: boolean } | null>(null);
  const errorRef = useRef<HTMLDivElement>(null);
  const id = useId();

  useEffect(() => {
    let cancelled = false;
    review.newerRevisions(course.id).then(
      (n) => {
        if (cancelled) return;
        setNotice(n);
        setChosen(new Set(n.steps.map((s) => s.lessonId)));
      },
      () => !cancelled && setLoadError(true),
    );
    return () => {
      cancelled = true;
    };
  }, [review, course.id]);
  useEffect(() => {
    if (error) errorRef.current?.focus();
  }, [error]);

  if (loadError)
    return (
      <p className="banner error" role="alert">
        Impossible de vérifier si des leçons du cours ont des révisions plus récentes. Rechargez la page pour réessayer.
      </p>
    );
  if (!notice || notice.steps.length === 0) return null;

  const create = async () => {
    setBusy(true);
    setError(null);
    try {
      const steps = notice.steps.filter((s) => chosen.has(s.lessonId)).map((s) => ({ lessonId: s.lessonId, revisionId: s.newerRevisionId }));
      onAmendment(await review.createAmendment(course.id, steps));
    } catch (e) {
      setError({ text: describeCourseWorkflowError(e), draftExists: isApiError(e, 'draft_exists') });
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="status-card warning" aria-labelledby={`${id}-heading`}>
      <h2 id={`${id}-heading`}>Des leçons du cours publié ont une révision plus récente</h2>
      <p>
        Le cours publié épingle des révisions de leçons plus anciennes que celles qui sont maintenant publiées. Les élèves lisent toujours les révisions épinglées : rien ne change sans un amendement
        relu et publié. Un amendement ne remplace que les révisions choisies ; le relecteur ne verra que ces étapes comme modifiées.
      </p>
      <fieldset>
        <legend>Révisions à remplacer</legend>
        {notice.steps.map((s) => (
          <div key={s.lessonId} className="checkbox">
            <input
              type="checkbox"
              id={`${id}-${s.lessonId}`}
              checked={chosen.has(s.lessonId)}
              onChange={(e) =>
                setChosen((c) => {
                  const next = new Set(c);
                  if (e.target.checked) next.add(s.lessonId);
                  else next.delete(s.lessonId);
                  return next;
                })
              }
            />
            <label htmlFor={`${id}-${s.lessonId}`}>
              « {s.title} » : révision {s.pinnedNumber} épinglée, révision {s.newerNumber} disponible
            </label>
          </div>
        ))}
      </fieldset>
      {draftOpen && <p className="hint">Un brouillon de ce cours est ouvert : un amendement ne peut être créé qu’après sa suppression. Vous pouvez aussi remplacer la révision dans ce brouillon.</p>}
      <div className="actions">
        <button type="button" className="primary" onClick={() => void create()} disabled={busy || expired || chosen.size === 0}>
          Créer un amendement
        </button>
      </div>
      {error && (
        <div className="banner error" role="alert" tabIndex={-1} ref={errorRef}>
          <p>{error.text}</p>
          {error.draftExists && !draftOpen && <p>Rechargez la page pour ouvrir le brouillon existant.</p>}
        </div>
      )}
    </section>
  );
}

/** The owner asks administrators to archive a submitted course, with a reason (recorded and listed for them). */
export function ArchiveRequest({ course }: { course: Course }) {
  const { api, expired } = useStudio();
  const review = useMemo(() => courseReviewApi(api), [api]);
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState('');
  const [fieldError, setFieldError] = useState<string | null>(null);
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  const field = useRef<HTMLTextAreaElement>(null);
  const doneRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (open) field.current?.focus();
  }, [open]);
  useEffect(() => {
    if (done) doneRef.current?.focus();
  }, [done]);

  if (course.archived) return null;
  if (done)
    return (
      <p className="banner success" role="status" tabIndex={-1} ref={doneRef}>
        Demande d’archivage envoyée aux administrateurs avec votre motif. Le cours reste dans son état actuel jusqu’à leur décision.
      </p>
    );

  const send = async () => {
    setBusy(true);
    try {
      await review.requestArchive(course.id, reason.trim());
      setAsking(false);
      setDone(true);
    } catch (e) {
      setAsking(false);
      setError(describeCourseWorkflowError(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="danger-zone" aria-labelledby="archive-request-heading">
      <h2 id="archive-request-heading">Archivage</h2>
      <p className="hint">Un cours déjà soumis n’est jamais supprimé. Vous pouvez demander son archivage : un administrateur le retire alors du catalogue et de la lecture, en conservant son historique.</p>
      {!open ? (
        <button type="button" onClick={() => setOpen(true)} disabled={expired}>
          Demander l’archivage…
        </button>
      ) : (
        <div className="reason-form">
          <NoteField
            id="archive-request-reason"
            label="Motif de la demande"
            hint="Obligatoire, transmis aux administrateurs et conservé dans l’historique (2 000 caractères au maximum)."
            value={reason}
            onChange={(v) => {
              setReason(v);
              if (fieldError && v.trim()) setFieldError(null);
            }}
            error={fieldError}
            fieldRef={field}
          />
          <div className="actions">
            <button
              type="button"
              className="danger"
              disabled={busy || expired}
              onClick={() => {
                setError(null);
                if (!reason.trim()) {
                  setFieldError('Indiquez le motif de la demande : il est obligatoire.');
                  field.current?.focus();
                  return;
                }
                setAsking(true);
              }}
            >
              Envoyer la demande…
            </button>
            <button type="button" onClick={() => setOpen(false)}>
              Annuler
            </button>
          </div>
        </div>
      )}
      {error && (
        <p className="banner error" role="alert">
          {error}
        </p>
      )}
      {asking && (
        <Dialog title="Demander l’archivage du cours ?" onCancel={() => setAsking(false)} cancelLabel="Ne pas envoyer" actions={[{ label: 'Envoyer la demande', kind: 'danger', disabled: busy, onClick: () => void send() }]}>
          <p>Les administrateurs recevront votre demande avec ce motif :</p>
          <Quote>{reason.trim()}</Quote>
        </Dialog>
      )}
    </section>
  );
}
