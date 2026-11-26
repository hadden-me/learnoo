// Administrator course recovery (LMS-B06): course revisions no one may
// review (named gaps), owners' archive requests, every course, and one
// course's page with its revisions, audit trail and the recovery actions:
// suspend, restore, archive, unarchive, withdraw a revision, discard a
// draft and transfer. Each action asks for a reason (restore has none in
// the API) and a confirmation; pointer actions name the published revision
// this page loaded, so a concurrent change is refused (stale_pointer) and
// nothing changes. No page approves anything on a reviewer's behalf.
import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { isApiError } from '../api/client';
import { courseReviewApi, type AdminCourse, type AdminCourseDetail, type AdminCourseRevision } from '../api/courseReview';
import { describeCourseWorkflowError } from '../api/courseReviewMessages';
import { useStudio } from '../context';
import { courseState, courseTitle } from '../courses/courseLabels';
import { fold } from '../courses/outline';
import { adminCourseActions, describeBlocked, lifecycleLabel, pointerLabel } from '../courses/workflow';
import { COURSE_STATUS_LABELS } from '../courses/courseLabels';
import { paths } from '../editorialRoutes';
import { LANGUAGE_LABELS, formatDateTime } from '../labels';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { ErrorPage, LoadingPage, NoteField, Quote, dirOf, useLoad } from './common';

function useCourseAdmin() {
  const { api } = useStudio();
  return useMemo(() => courseReviewApi(api), [api]);
}

/** Lessons / courses switch of the administration space. */
export function AdminTabs({ current }: { current: 'lessons' | 'courses' }) {
  return (
    <nav className="tabs" aria-label="Administration">
      <Link to={paths.adminLessons} current={current === 'lessons'}>
        Leçons
      </Link>
      <Link to={paths.adminCourses} current={current === 'courses'}>
        Cours
      </Link>
    </nav>
  );
}

function CourseStateTag({ course }: { course: AdminCourse }) {
  const state = courseState(course);
  return <span className={`state state-${state.tone}`}>{state.label}</span>;
}

export function AdminCoursesPage() {
  const cr = useCourseAdmin();
  const [blocked] = useLoad(() => cr.blockedReviews(), [cr]);
  const [requests] = useLoad(() => cr.archiveRequests(), [cr]);
  const [list, setList] = useState<AdminCourse[] | null>(null);
  const [next, setNext] = useState<string | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const load = async (cursor: string | null) => {
    setLoadingMore(true);
    setListError(null);
    try {
      const page = await cr.adminCourses(cursor);
      setList((prev) => (cursor === null ? page.courses : [...(prev ?? []), ...page.courses]));
      setNext(page.nextCursor);
    } catch (e) {
      setListError(describeCourseWorkflowError(e));
    } finally {
      setLoadingMore(false);
    }
  };
  useEffect(() => {
    void load(null);
  }, [cr]);

  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>Administration des cours</h1>
      <AdminTabs current="courses" />
      <p className="hint">Suspension, archivage, restauration, retrait d’une version, suppression d’un brouillon et transfert de propriété. Chaque action est enregistrée avec son motif. Aucune action n’approuve un cours à la place d’un relecteur.</p>

      <section aria-labelledby="blocked-reviews-heading">
        <h2 id="blocked-reviews-heading">Relectures bloquées</h2>
        {blocked.state === 'loading' && <p role="status">Chargement…</p>}
        {blocked.state === 'error' && (
          <p className="banner error" role="alert">
            {blocked.message}
          </p>
        )}
        {blocked.state === 'ready' && blocked.data.revisions.length === 0 && <p>Aucune version de cours en relecture n’est bloquée.</p>}
        {blocked.state === 'ready' && blocked.data.revisions.length > 0 && (
          <ul className="lesson-list" aria-label="Versions de cours sans relecteur éligible">
            {blocked.data.revisions.map((item) => {
              const b = item.blocked ? describeBlocked(item.blocked) : null;
              return (
                <li key={item.revisionId} className="lesson-card">
                  <h3 lang={item.language} dir={dirOf(item.language)}>
                    <Link to={paths.adminCourse(item.courseId)}>{courseTitle(item.title, item.courseId)}</Link>
                  </h3>
                  <p className="lesson-meta">
                    <span className="state state-changes">Bloqué : aucun relecteur éligible</span>
                  </p>
                  <p className="lesson-context">
                    Version {item.number} · par {item.authorName} · soumise le {formatDateTime(item.submittedAt)}
                  </p>
                  {b && (
                    <div className="lesson-context">
                      <p>{b.summary}</p>
                      {b.details.length > 0 && (
                        <ul>
                          {b.details.map((d) => (
                            <li key={d}>{d}</li>
                          ))}
                        </ul>
                      )}
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </section>

      <section aria-labelledby="archive-requests-heading">
        <h2 id="archive-requests-heading">Demandes d’archivage</h2>
        {requests.state === 'loading' && <p role="status">Chargement…</p>}
        {requests.state === 'error' && (
          <p className="banner error" role="alert">
            {requests.message}
          </p>
        )}
        {requests.state === 'ready' && requests.data.length === 0 && <p>Aucune demande d’archivage.</p>}
        {requests.state === 'ready' && requests.data.length > 0 && (
          <ul className="lesson-list" aria-label="Demandes d’archivage">
            {requests.data.map((r) => (
              <li key={r.id} className="lesson-card">
                <h3>
                  <Link to={paths.adminCourse(r.courseId)}>{courseTitle(r.title, r.courseId)}</Link>
                </h3>
                <p className="lesson-meta">{r.archived ? <span className="state">Déjà archivé</span> : <span className="state state-review">À traiter</span>}</p>
                <p className="lesson-context">Demandée le {formatDateTime(r.occurredAt)} :</p>
                <Quote>{r.reason}</Quote>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section aria-labelledby="all-courses-heading">
        <h2 id="all-courses-heading">Tous les cours</h2>
        {listError && (
          <div className="banner error" role="alert">
            <p>{listError}</p>
            <button type="button" onClick={() => void load(null)}>
              Réessayer
            </button>
          </div>
        )}
        {list === null && !listError && <p role="status">Chargement des cours…</p>}
        {list !== null && list.length === 0 && <p>Aucun cours.</p>}
        {list !== null && list.length > 0 && (
          <ul className="lesson-list" aria-label="Tous les cours">
            {list.map((c) => (
              <li key={c.id} className="lesson-card">
                <h3 lang={c.language} dir={dirOf(c.language)}>
                  <Link to={paths.adminCourse(c.id)}>{courseTitle(c.title, c.id)}</Link>
                </h3>
                <p className="lesson-meta">
                  <CourseStateTag course={c} />
                  {c.publishedRevisionId !== null && <span className="state state-published">Lisible par les élèves</span>}
                </p>
                <p className="lesson-context">
                  {LANGUAGE_LABELS[c.language]} · propriétaire : {c.ownerName}
                </p>
              </li>
            ))}
          </ul>
        )}
        {next && (
          <button type="button" onClick={() => void load(next)} disabled={loadingMore}>
            Afficher plus de cours
          </button>
        )}
      </section>
    </main>
  );
}

type ActionKind = 'suspend' | 'restore' | 'archive' | 'unarchive' | 'withdraw' | 'discard' | 'transfer';

interface Pending {
  kind: ActionKind;
  revision?: AdminCourseRevision;
}

const ACTION_TITLES: Record<ActionKind, string> = {
  suspend: 'Suspendre le cours',
  restore: 'Restaurer une version',
  archive: 'Archiver le cours',
  unarchive: 'Désarchiver le cours',
  withdraw: 'Retirer une version',
  discard: 'Supprimer le brouillon',
  transfer: 'Transférer le cours',
};

const ACTION_EFFECTS: Record<ActionKind, string> = {
  suspend: 'Le cours disparaît immédiatement du catalogue et de la lecture. Son historique et les favoris sont conservés. Pour lever la suspension, restaurez une version publiée auparavant : chaque leçon est alors vérifiée de nouveau.',
  restore: 'Cette version redevient celle que lisent les élèves, si toutes ses leçons sont encore publiables ; sinon le serveur refuse et rien ne change.',
  archive: 'Le cours est retiré du catalogue et de la lecture ; son historique est conservé. Le désarchivage ne le republiera pas automatiquement.',
  unarchive: 'Le cours n’est plus archivé, mais il reste illisible : il faudra une restauration ou une nouvelle publication.',
  withdraw: 'Cette version ne pourra plus jamais être relue, publiée ni restaurée. Le cours peut alors recevoir une nouvelle version.',
  discard: 'Le brouillon ouvert est supprimé définitivement, sans toucher aux leçons ni aux versions soumises. À utiliser quand le propriétaire ne peut pas le faire.',
  transfer: 'Le nouveau propriétaire rédigera les prochaines versions. L’historique garde ses auteurs ; les leçons de l’ancien propriétaire ne deviennent pas modifiables par le nouveau.',
};

function revisionState(d: AdminCourseDetail, r: AdminCourseRevision): string {
  if (r.withdrawnAt) return 'Retirée';
  if (d.course.publishedRevisionId === r.id) return 'Publiée (lue par les élèves)';
  if (r.status === 'approved' && r.everPublished) return 'Approuvée, publiée auparavant';
  if (r.status === 'approved') return 'Approuvée, en attente de publication';
  return COURSE_STATUS_LABELS[r.status];
}

export function AdminCoursePage({ courseId }: { courseId: number }) {
  const cr = useCourseAdmin();
  const [loadedAt, setLoadedAt] = useState(() => new Date());
  const [loaded, reload] = useLoad(async () => {
    const [detail, newer] = await Promise.all([
      cr.adminCourse(courseId),
      cr.newerRevisions(courseId).then(
        (n) => ({ ok: true as const, n }),
        () => ({ ok: false as const, n: null }),
      ),
    ]);
    setLoadedAt(new Date());
    return { detail, newer: newer.n, newerFailed: !newer.ok };
  }, [cr, courseId]);
  const [pending, setPending] = useState<Pending | null>(null);
  const [reason, setReason] = useState('');
  const [reasonError, setReasonError] = useState<string | null>(null);
  const [target, setTarget] = useState('');
  const [authorQuery, setAuthorQuery] = useState('');
  const [targetError, setTargetError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ text: string; stale: boolean } | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const reasonRef = useRef<HTMLTextAreaElement>(null);
  const resultRef = useRef<HTMLDivElement>(null);
  const formId = useId();

  useEffect(() => {
    if (pending) reasonRef.current?.focus();
  }, [pending]);
  useEffect(() => {
    if (done || error) resultRef.current?.focus();
  }, [done, error]);

  if (loaded.state === 'loading') return <LoadingPage label="Chargement du cours…" />;
  if (loaded.state === 'error')
    return <ErrorPage title="Cours introuvable" message={loaded.notFound ? 'Aucun cours ne correspond à ce numéro.' : loaded.message} back={{ to: paths.adminCourses, label: 'Revenir à l’administration des cours' }} />;
  const { detail, newer, newerFailed } = loaded.data;
  const c = detail.course;
  const actions = adminCourseActions(detail);
  const byId = new Map(detail.revisions.map((r) => [r.id, r]));
  const numberOf = (id: number | null) => (id === null ? '—' : byId.has(id) ? `version ${byId.get(id)!.number}` : `version n° ${id}`);
  const needsReason = (k: ActionKind) => k !== 'restore';
  const q = fold(authorQuery.trim());
  const matchingAuthors = detail.authors.filter((a) => q === '' || fold(a.displayName).includes(q) || String(a.id) === target);

  const open = (p: Pending) => {
    setPending(p);
    setReason('');
    setReasonError(null);
    setTarget('');
    setAuthorQuery('');
    setTargetError(null);
    setError(null);
    setDone(null);
  };

  const check = () => {
    if (!pending) return;
    let ok = true;
    if (pending.kind === 'transfer' && !target) {
      setTargetError('Choisissez le nouveau propriétaire.');
      ok = false;
    }
    if (needsReason(pending.kind) && !reason.trim()) {
      setReasonError('Indiquez un motif : il est obligatoire et sera conservé dans l’historique.');
      reasonRef.current?.focus();
      ok = false;
    }
    if (ok) setConfirming(true);
  };

  const run = async () => {
    if (!pending) return;
    setBusy(true);
    const text = reason.trim();
    const expected = c.publishedRevisionId;
    try {
      let message: string;
      switch (pending.kind) {
        case 'suspend':
          await cr.suspend(c.id, text, expected);
          message = 'Cours suspendu : il n’est plus lisible par les élèves.';
          break;
        case 'restore':
          await cr.restore(c.id, pending.revision!.id, expected);
          message = `Version ${pending.revision!.number} restaurée : les élèves la lisent de nouveau.`;
          break;
        case 'archive':
          await cr.archive(c.id, text, expected);
          message = 'Cours archivé.';
          break;
        case 'unarchive':
          await cr.unarchive(c.id, text);
          message = 'Cours désarchivé. Il reste illisible jusqu’à une restauration ou une nouvelle publication.';
          break;
        case 'withdraw':
          await cr.withdraw(pending.revision!.id, text);
          message = `Version ${pending.revision!.number} retirée.`;
          break;
        case 'discard':
          await cr.discardDraft(pending.revision!.id, text);
          message = `Brouillon (version ${pending.revision!.number}) supprimé.`;
          break;
        case 'transfer': {
          const to = detail.authors.find((a) => a.id === Number(target));
          await cr.transfer(c.id, Number(target), text);
          message = `Cours transféré à ${to?.displayName ?? `compte n° ${target}`}.`;
          break;
        }
      }
      setConfirming(false);
      setPending(null);
      setDone(message);
      await reload();
    } catch (e) {
      setConfirming(false);
      setError({ text: describeCourseWorkflowError(e), stale: isApiError(e, 'stale_pointer') });
    } finally {
      setBusy(false);
    }
  };

  const revisionButtons = (r: AdminCourseRevision) => (
    <div className="actions">
      {actions.restore.includes(r) && (
        <button type="button" onClick={() => open({ kind: 'restore', revision: r })}>
          Restaurer la version {r.number}…
        </button>
      )}
      {actions.withdraw.includes(r) && (
        <button type="button" className="danger" onClick={() => open({ kind: 'withdraw', revision: r })}>
          Retirer la version {r.number}…
        </button>
      )}
      {actions.discard.includes(r) && (
        <button type="button" className="danger" onClick={() => open({ kind: 'discard', revision: r })}>
          Supprimer le brouillon {r.number}…
        </button>
      )}
    </div>
  );

  const pendingTitle = pending ? `${ACTION_TITLES[pending.kind]}${pending.revision ? ` (version ${pending.revision.number})` : ''}` : '';

  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={paths.adminCourses}>Administration des cours</Link>
      </p>
      <h1 tabIndex={-1} lang={c.language} dir={dirOf(c.language)}>
        {courseTitle(c.title, c.id)}
      </h1>
      <p className="lesson-meta">
        <CourseStateTag course={c} />
      </p>
      <dl className="facts">
        <div>
          <dt>Propriétaire</dt>
          <dd>{c.ownerName}</dd>
        </div>
        <div>
          <dt>Langue</dt>
          <dd>{LANGUAGE_LABELS[c.language]}</dd>
        </div>
        <div>
          <dt>Version lue par les élèves</dt>
          <dd>{c.publishedRevisionId === null ? 'aucune' : numberOf(c.publishedRevisionId)}</dd>
        </div>
        <div>
          <dt>État chargé à</dt>
          <dd>{formatDateTime(loadedAt.toISOString())}</dd>
        </div>
      </dl>

      <div aria-live="polite">
        {done && (
          <div className="banner success" role="status" tabIndex={-1} ref={resultRef}>
            <p>{done}</p>
          </div>
        )}
        {error && (
          <div className="banner error" role="alert" tabIndex={-1} ref={resultRef}>
            <p>{error.text}</p>
            {error.stale && (
              <button type="button" onClick={() => { setError(null); void reload(); }}>
                Recharger l’état actuel
              </button>
            )}
          </div>
        )}
      </div>

      {newerFailed && (
        <div className="banner error" role="alert">
          <p>Impossible de vérifier si des leçons du cours ont des révisions plus récentes.</p>
          <button type="button" onClick={() => void reload()}>
            Réessayer
          </button>
        </div>
      )}
      {newer && newer.steps.length > 0 && (
        <section className="status-card warning" aria-labelledby="newer-heading">
          <h2 id="newer-heading">Des leçons du cours publié ont une révision plus récente</h2>
          <ul>
            {newer.steps.map((s) => (
              <li key={s.lessonId}>
                « {s.title} » : révision {s.pinnedNumber} épinglée, révision {s.newerNumber} disponible
              </li>
            ))}
          </ul>
          <p>Seul le propriétaire peut créer l’amendement qui les remplace ; il sera relu comme toute version.</p>
        </section>
      )}

      <section className="status-card safeguard" aria-labelledby="course-actions-heading">
        <h2 id="course-actions-heading">Actions sur le cours</h2>
        <div className="actions">
          {actions.suspend && (
            <button type="button" className="danger" onClick={() => open({ kind: 'suspend' })}>
              Suspendre le cours…
            </button>
          )}
          {actions.archive && (
            <button type="button" className="danger" onClick={() => open({ kind: 'archive' })}>
              Archiver le cours…
            </button>
          )}
          {actions.unarchive && (
            <button type="button" onClick={() => open({ kind: 'unarchive' })}>
              Désarchiver le cours…
            </button>
          )}
          <button type="button" onClick={() => open({ kind: 'transfer' })} disabled={!actions.transfer} aria-describedby={actions.transfer ? undefined : `${formId}-transfer`}>
            Transférer le cours…
          </button>
        </div>
        {!actions.transfer && (
          <p className="hint" id={`${formId}-transfer`}>
            Transfert impossible tant qu’une version est en cours (brouillon, en relecture ou approuvée non publiée) : supprimez le brouillon ou retirez la version d’abord.
          </p>
        )}
        {pending && (
          <form
            className="reason-form"
            aria-labelledby={`${formId}-title`}
            onSubmit={(e) => {
              e.preventDefault();
              check();
            }}
            noValidate
          >
            <h3 id={`${formId}-title`}>{pendingTitle}</h3>
            <p>{ACTION_EFFECTS[pending.kind]}</p>
            {pending.kind === 'transfer' && (
              <div className="fields">
                <label htmlFor={`${formId}-search`}>Rechercher un auteur</label>
                <input id={`${formId}-search`} type="search" value={authorQuery} onChange={(e) => setAuthorQuery(e.target.value)} aria-describedby={`${formId}-count`} />
                <p className="hint" id={`${formId}-count`} role="status">
                  {matchingAuthors.length} auteur{matchingAuthors.length > 1 ? 's' : ''} actif{matchingAuthors.length > 1 ? 's' : ''}
                  {detail.authors.length >= 200 ? ' (200 premiers par nom : précisez la recherche)' : ''}
                </p>
                <label htmlFor={`${formId}-target`}>Nouveau propriétaire</label>
                {targetError && (
                  <p className="field-error" id={`${formId}-target-error`}>
                    {targetError}
                  </p>
                )}
                <select
                  id={`${formId}-target`}
                  value={target}
                  aria-invalid={targetError ? true : undefined}
                  aria-describedby={targetError ? `${formId}-target-error` : undefined}
                  onChange={(e) => {
                    setTarget(e.target.value);
                    setTargetError(null);
                  }}
                >
                  <option value="">Choisir un auteur…</option>
                  {matchingAuthors.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.displayName}
                    </option>
                  ))}
                </select>
              </div>
            )}
            {needsReason(pending.kind) && (
              <NoteField
                id={`${formId}-reason`}
                label="Motif"
                hint="Obligatoire, conservé dans l’historique (2 000 caractères au maximum)."
                value={reason}
                onChange={(v) => {
                  setReason(v);
                  if (reasonError && v.trim()) setReasonError(null);
                }}
                error={reasonError}
                fieldRef={reasonRef}
              />
            )}
            <div className="actions">
              <button type="submit" className={pending.kind === 'restore' || pending.kind === 'unarchive' ? 'primary' : 'danger'}>
                Continuer…
              </button>
              <button type="button" onClick={() => setPending(null)}>
                Annuler
              </button>
            </div>
          </form>
        )}
      </section>

      <section className="panel" aria-labelledby="revisions-heading">
        <h2 id="revisions-heading">Versions du cours</h2>
        {detail.revisions.length === 0 ? (
          <p>Aucune version.</p>
        ) : (
          <ul className="lesson-list" aria-label="Versions du cours">
            {detail.revisions.map((r) => (
              <li key={r.id} className="lesson-card">
                <h3>
                  Version {r.number} · {revisionState(detail, r)}
                </h3>
                <p className="lesson-context">
                  {r.kind === 'amendment' ? 'Amendement' : 'Version ordinaire'} · par {r.authorName}
                  {r.submittedAt && ` · soumise le ${formatDateTime(r.submittedAt)}`}
                  {r.review && ` · ${r.review.action === 'approve' ? 'approuvée' : 'modifications demandées'} par ${r.review.reviewerName}`}
                </p>
                {(r.status === 'in_review' || r.status === 'approved') && r.withdrawnAt === null && (
                  <p className="lesson-context">
                    <Link to={paths.reviewCourse(r.id)}>Lire cette version</Link>
                  </p>
                )}
                {revisionButtons(r)}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="panel" aria-labelledby="history-heading">
        <h2 id="history-heading">Historique</h2>
        {detail.history.length === 0 && detail.lifecycle.length === 0 ? (
          <p>Aucun évènement de publication ni de cycle de vie.</p>
        ) : (
          <ul className="history" aria-label="Historique du cours">
            {[
              ...detail.history.map((e, i) => ({ at: e.occurredAt, key: `p-${i}`, label: `${pointerLabel(e.action)}${e.publishedRevisionId !== null ? ` de la ${numberOf(e.publishedRevisionId)}` : ''}`, actor: e.actorName, reason: e.reason })),
              ...detail.lifecycle.map((e) => ({ at: e.occurredAt, key: `l-${e.id}`, label: lifecycleLabel(e.action), actor: e.actorName, reason: e.reason })),
            ]
              .sort((a, b) => b.at.localeCompare(a.at))
              .map((e) => (
                <li key={e.key}>
                  <p>
                    {e.label} · {e.actor} · {formatDateTime(e.at)}
                  </p>
                  {e.reason && <Quote>{e.reason}</Quote>}
                </li>
              ))}
          </ul>
        )}
      </section>

      {confirming && pending && (
        <Dialog
          title={`${pendingTitle} ?`}
          onCancel={() => setConfirming(false)}
          cancelLabel="Ne rien faire"
          actions={[{ label: ACTION_TITLES[pending.kind], kind: pending.kind === 'restore' || pending.kind === 'unarchive' ? 'primary' : 'danger', disabled: busy, onClick: () => void run() }]}
        >
          <p>{ACTION_EFFECTS[pending.kind]}</p>
          {needsReason(pending.kind) && (
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
