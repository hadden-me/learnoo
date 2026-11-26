// Reviewer space: the queue of lessons and courses in review in the
// reviewer's subjects (GET /api/review/queue and ?type=course), filtered by
// content type, subject and decision, a page at a time, and the review of
// one lesson revision (courses: CourseReviewPages.tsx), read
// through the canonical preview, with approve (optional comment) and
// request changes (required comment), each confirmed first. The server
// decides who may review what (assignment per subject, no self-review);
// this page only explains its answers.
import { useId, useMemo, useRef, useState } from 'react';
import { isApiError, type Revision, type Subject } from '../api/client';
import type { ReviewQueueItem } from '../api/editorial';
import { courseReviewApi, type CourseReviewItem } from '../api/courseReview';
import { describeBlocked, filterQueue, mergeQueue, paginate, submissionAge, type QueueDecision, type QueueType } from '../courses/workflow';
import { describeEditorialError } from '../api/editorialMessages';
import { setFlash, takeFlash, useStudio } from '../context';
import { paths } from '../editorialRoutes';
import { LANGUAGE_LABELS, formatDateTime } from '../labels';
import { navigate } from '../router';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { CatalogueName, catalogueLabel, entryLabel, type CatalogueLabel } from '../catalogue';
import { CanonicalPreview, ErrorPage, LoadingPage, NoteField, Quote, SubjectName, dirOf, subjectLabel, useEditorial, useLoad, useSubjects } from './common';

/** Queue items grouped by subject, in catalogue order (display order, then code); subjects missing from the catalogue last, by ID. */
/** Queue page size: large, as the queues are bounded to 200 each by the server. */
export const QUEUE_PAGE_SIZE = 50;

export function groupBySubject<T extends { subjectId: number }>(items: T[], subjects: Map<number, Subject>): Array<{ subjectId: number; items: T[] }> {
  const groups = new Map<number, T[]>();
  for (const item of items) groups.set(item.subjectId, [...(groups.get(item.subjectId) ?? []), item]);
  const rank = (id: number) => {
    const s = subjects.get(id);
    return s ? [0, s.sortOrder, s.code] as const : [1, 0, ''] as const;
  };
  return [...groups.entries()]
    .sort(([a], [b]) => {
      const [ka, oa, ca] = rank(a);
      const [kb, ob, cb] = rank(b);
      return ka - kb || oa - ob || (ca < cb ? -1 : ca > cb ? 1 : 0) || a - b;
    })
    .map(([subjectId, list]) => ({ subjectId, items: list }));
}

export function ReviewQueuePage() {
  const ed = useEditorial();
  const { api } = useStudio();
  const cr = useMemo(() => courseReviewApi(api), [api]);
  const subjects = useSubjects();
  const [queue] = useLoad(async () => {
    const [lessons, courses] = await Promise.all([ed.reviewQueue(), cr.reviewQueue()]);
    return mergeQueue(lessons, courses);
  }, [ed, cr]);
  const [flash] = useState(takeFlash);
  const [type, setType] = useState<QueueType>('all');
  const [subjectId, setSubjectId] = useState<number | null>(null);
  const [decision, setDecision] = useState<QueueDecision>('all');
  const [page, setPage] = useState(0);
  const ids = { type: useId(), subject: useId(), decision: useId(), results: useId() };
  const listTop = useRef<HTMLHeadingElement>(null);

  const all = queue.state === 'ready' ? queue.data : [];
  const filtered = filterQueue(all, { type, subjectId, decision });
  const shown = paginate(filtered, page, QUEUE_PAGE_SIZE);
  const subjectIds = [...new Set(all.map((e) => e.subjectId))];
  const goTo = (p: number) => {
    setPage(p);
    requestAnimationFrame(() => listTop.current?.focus());
  };

  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>Relecture</h1>
      <p className="hint">
        Leçons et cours soumis dans les matières dont vous êtes relecteur, les plus anciens d’abord. Vos propres leçons n’y figurent pas. Les cours apparaissent tous, avec ce qui vous empêche
        éventuellement de les décider : l’auteur du cours et l’auteur d’une leçon ajoutée ou remplacée ne peuvent pas l’approuver.
      </p>
      {flash && (
        <p className="banner success" role="status">
          {flash}
        </p>
      )}
      {queue.state === 'loading' && <p role="status">Chargement de la file de relecture…</p>}
      {queue.state === 'error' && (
        <p className="banner error" role="alert">
          {queue.message}
        </p>
      )}
      {queue.state === 'ready' && all.length === 0 && <p>Aucune révision n’attend de relecture dans vos matières.</p>}
      {queue.state === 'ready' && all.length > 0 && (
        <>
          <form className="queue-filters" onSubmit={(e) => e.preventDefault()} aria-label="Filtrer la file de relecture">
            <div className="fields">
              <label htmlFor={ids.type}>Type de contenu</label>
              <select id={ids.type} value={type} onChange={(e) => { setType(e.target.value as QueueType); setPage(0); }}>
                <option value="all">Leçons et cours</option>
                <option value="lesson">Leçons</option>
                <option value="course">Cours</option>
              </select>
            </div>
            <div className="fields">
              <label htmlFor={ids.subject}>Matière</label>
              <select id={ids.subject} value={subjectId ?? ''} onChange={(e) => { setSubjectId(e.target.value ? Number(e.target.value) : null); setPage(0); }}>
                <option value="">Toutes vos matières</option>
                {subjectIds.map((id) => (
                  <option key={id} value={id}>
                    {subjectLabel(subjects, id)}
                  </option>
                ))}
              </select>
            </div>
            <div className="fields">
              <label htmlFor={ids.decision}>Décision</label>
              <select id={ids.decision} value={decision} onChange={(e) => { setDecision(e.target.value as QueueDecision); setPage(0); }}>
                <option value="all">Tout</option>
                <option value="mine">Je peux décider</option>
                <option value="other">Un autre relecteur doit décider</option>
                <option value="blocked">Bloqués : aucun relecteur éligible</option>
              </select>
            </div>
          </form>
          <p id={ids.results} role="status">
            {filtered.length === 0
              ? 'Aucun élément ne correspond à ces filtres.'
              : `${filtered.length} élément${filtered.length > 1 ? 's' : ''}${shown.pages > 1 ? `, page ${shown.page + 1} sur ${shown.pages}` : ''}.`}
          </p>
          <h2 className="visually-hidden" tabIndex={-1} ref={listTop}>
            Résultats
          </h2>
          {groupBySubject(shown.items, subjects).map((group) => (
            <section key={group.subjectId} aria-labelledby={`subject-${group.subjectId}`}>
              <h2 id={`subject-${group.subjectId}`}>
                <SubjectName subjects={subjects} id={group.subjectId} /> ({group.items.length})
              </h2>
              <ul className="lesson-list" aria-label={`À relire en ${subjectLabel(subjects, group.subjectId)}`}>
                {group.items.map((entry) => (entry.type === 'lesson' ? <QueueCard key={entry.id} item={entry.item} /> : <CourseQueueCard key={entry.id} item={entry.item} />))}
              </ul>
            </section>
          ))}
          {shown.pages > 1 && (
            <nav className="pager" aria-label="Pages de la file de relecture">
              <button type="button" onClick={() => goTo(shown.page - 1)} disabled={shown.page === 0}>
                Page précédente
              </button>
              <span>
                Page {shown.page + 1} sur {shown.pages}
              </span>
              <button type="button" onClick={() => goTo(shown.page + 1)} disabled={shown.page >= shown.pages - 1}>
                Page suivante
              </button>
            </nav>
          )}
        </>
      )}
    </main>
  );
}

function QueueCard({ item }: { item: ReviewQueueItem }) {
  return (
    <li className="lesson-card">
      <h3 lang={item.language} dir={dirOf(item.language)}>
        <Link to={paths.reviewRevision(item.revisionId)}>{item.title || `Révision n° ${item.revisionId}`}</Link>
      </h3>
      <p className="lesson-meta">
        <span className="state kind-label">Leçon</span>
        <span className="state state-review">En relecture</span>
      </p>
      <p className="lesson-context">
        Révision {item.number} · {LANGUAGE_LABELS[item.language]} · par {item.authorName} · soumise le {formatDateTime(item.submittedAt)} ({submissionAge(item.submittedAt)})
      </p>
    </li>
  );
}

function CourseQueueCard({ item }: { item: CourseReviewItem }) {
  const blocked = item.blocked ? describeBlocked(item.blocked) : null;
  return (
    <li className="lesson-card">
      <h3 lang={item.language} dir={dirOf(item.language)}>
        <Link to={paths.reviewCourse(item.revisionId)}>{item.title || `Cours n° ${item.courseId}`}</Link>
      </h3>
      <p className="lesson-meta">
        <span className="state kind-label">{item.kind === 'amendment' ? 'Cours · amendement' : 'Cours'}</span>
        <span className="state state-review">En relecture</span>
        {blocked ? (
          <span className="state state-changes">Bloqué : aucun relecteur éligible</span>
        ) : item.eligible ? (
          <span className="state state-approved">Vous pouvez décider</span>
        ) : (
          <span className="state">Un autre relecteur doit décider</span>
        )}
      </p>
      <p className="lesson-context">
        Version {item.number} · {LANGUAGE_LABELS[item.language]} · par {item.authorName} · soumise le {formatDateTime(item.submittedAt)} ({submissionAge(item.submittedAt)})
      </p>
      {blocked && (
        <div className="lesson-context">
          <p>{blocked.summary}</p>
          {blocked.details.length > 0 && (
            <ul>
              {blocked.details.map((d) => (
                <li key={d}>{d}</li>
              ))}
            </ul>
          )}
        </div>
      )}
    </li>
  );
}

interface ReviewData {
  revision: Revision;
  authorName: string | null;
  subject: CatalogueLabel | null;
  chapter: CatalogueLabel | null;
}

type Decision = 'approve' | 'request_changes';

export function ReviewRevisionPage({ revisionId }: { revisionId: number }) {
  const { api, session } = useStudio();
  const ed = useEditorial();
  const [loaded] = useLoad<ReviewData>(async () => {
    const revision = await ed.reviewRevision(revisionId);
    // Context only: a failure here does not block the review.
    const [queue, subjects] = await Promise.all([ed.reviewQueue(revision.subjectId).catch(() => []), api.subjects().catch(() => [])]);
    // Names in the revision's language, else the other, else the code.
    const entry = subjects.find((s) => s.id === revision.subjectId);
    const subject = entry ? entryLabel(entry, revision.language) : null;
    let chapter: CatalogueLabel | null = null;
    if (entry && revision.chapterId !== null) {
      const c = (await api.chapters(entry.code).catch(() => null))?.chapters.find((ch) => ch.id === revision.chapterId);
      chapter = c ? catalogueLabel(c.code, c.nameFr, c.nameAr, revision.language) : null;
    }
    return { revision, authorName: queue.find((q) => q.revisionId === revisionId)?.authorName ?? null, subject, chapter };
  }, [ed, api, revisionId]);

  if (loaded.state === 'loading') return <LoadingPage label="Chargement de la révision…" />;
  if (loaded.state === 'error')
    return (
      <ErrorPage
        title="Révision indisponible"
        message={
          loaded.notFound
            ? 'Cette révision n’est pas en relecture dans l’une de vos matières : elle n’existe pas, a déjà été décidée, ou relève d’une matière que vous ne relisez pas.'
            : loaded.message
        }
        back={{ to: paths.reviewQueue, label: 'Revenir à la file de relecture' }}
      />
    );
  const { revision, authorName, subject, chapter } = loaded.data;
  const own = revision.authorId === session.user.id;
  const dir = dirOf(revision.language);
  return (
    <main className="page" id="main">
      <p className="breadcrumb">
        <Link to={paths.reviewQueue}>Relecture</Link>
      </p>
      <h1 tabIndex={-1} lang={revision.language} dir={dir}>
        {revision.title}
      </h1>
      <p className="lesson-meta">
        <span className="state state-review">En relecture</span>
      </p>
      <dl className="facts">
        <div>
          <dt>Auteur</dt>
          <dd>{own ? 'Vous' : (authorName ?? `compte n° ${revision.authorId}`)}</dd>
        </div>
        <div>
          <dt>Matière</dt>
          <dd>
            {subject ? <CatalogueName label={subject} /> : `matière n° ${revision.subjectId}`}
            {revision.chapterId !== null && (
              <>
                {' · '}
                {chapter ? <CatalogueName label={chapter} /> : `chapitre n° ${revision.chapterId}`}
              </>
            )}
          </dd>
        </div>
        <div>
          <dt>Langue</dt>
          <dd>{LANGUAGE_LABELS[revision.language]}</dd>
        </div>
        <div>
          <dt>Révision</dt>
          <dd>
            n° {revision.number}
            {revision.submittedAt && `, soumise le ${formatDateTime(revision.submittedAt)}`}
          </dd>
        </div>
        {revision.prerequisites && (
          <div>
            <dt>Prérequis</dt>
            <dd lang={revision.language} dir={dir} className="pre-line">
              {revision.prerequisites}
            </dd>
          </div>
        )}
      </dl>
      {own ? (
        <section className="status-card" aria-labelledby="own-heading">
          <h2 id="own-heading">Vous êtes l’auteur de cette révision</h2>
          <p>Vous pouvez la lire, mais pas la relire : le serveur refuse qu’un auteur approuve ou renvoie sa propre révision. Un autre relecteur de la matière doit s’en charger.</p>
        </section>
      ) : (
        <DecisionForm revision={revision} />
      )}
      {revision.mediaIds.length > 0 && (
        <section className="panel" aria-labelledby="media-heading">
          <h2 id="media-heading">Images jointes ({revision.mediaIds.length})</h2>
          <ul className="media-list" aria-label="Images jointes">
            {revision.mediaIds.map((id) => (
              <li key={id}>
                <img src={`/api/review/media/${id}`} alt={`Image jointe n° ${id}`} />
                <span>Image n° {id}</span>
              </li>
            ))}
          </ul>
        </section>
      )}
      <section className="panel" aria-labelledby="preview-heading">
        <h2 id="preview-heading">Leçon telle que l’élève la lira</h2>
        <CanonicalPreview content={revision.content} testId="review-preview" mediaRoute="review" />
      </section>
    </main>
  );
}

function DecisionForm({ revision }: { revision: Revision }) {
  const ed = useEditorial();
  const [comment, setComment] = useState('');
  const [fieldError, setFieldError] = useState<string | null>(null);
  const [asking, setAsking] = useState<Decision | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const field = useRef<HTMLTextAreaElement>(null);

  const ask = (decision: Decision) => {
    setError(null);
    if (decision === 'request_changes' && !comment.trim()) {
      setFieldError('Expliquez les modifications demandées : le commentaire est obligatoire pour renvoyer la révision.');
      field.current?.focus();
      return;
    }
    setFieldError(null);
    setAsking(decision);
  };

  const decide = async (decision: Decision) => {
    setBusy(true);
    try {
      if (decision === 'approve') await ed.approve(revision.id, comment);
      else await ed.requestChanges(revision.id, comment);
      setFlash(
        decision === 'approve'
          ? `Révision approuvée : « ${revision.title} ». Elle attend maintenant sa publication.`
          : `Modifications demandées pour « ${revision.title} ». L’auteur verra votre commentaire.`,
      );
      navigate(paths.reviewQueue, { force: true });
    } catch (e) {
      setAsking(null);
      setError(describeEditorialError(e));
      if (isApiError(e, 'comment_required') || isApiError(e, 'invalid_comment')) field.current?.focus();
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="status-card decision" aria-labelledby="decision-heading">
      <h2 id="decision-heading">Votre décision</h2>
      <NoteField
        id="review-comment"
        label="Commentaire pour l’auteur"
        hint="Facultatif pour approuver, obligatoire pour demander des modifications (5 000 caractères au maximum)."
        value={comment}
        onChange={(v) => {
          setComment(v);
          if (fieldError && v.trim()) setFieldError(null);
        }}
        error={fieldError}
        fieldRef={field}
      />
      <div className="actions">
        <button type="button" className="primary" onClick={() => ask('approve')} disabled={busy}>
          Approuver…
        </button>
        <button type="button" onClick={() => ask('request_changes')} disabled={busy}>
          Demander des modifications…
        </button>
      </div>
      {error && (
        <div className="banner error" role="alert">
          <p>{error}</p>
          <p>
            <Link to={paths.reviewQueue}>Revenir à la file de relecture</Link>
          </p>
        </div>
      )}
      {asking && (
        <Dialog
          title={asking === 'approve' ? `Approuver la révision ${revision.number} ?` : `Demander des modifications ?`}
          onCancel={() => setAsking(null)}
          cancelLabel="Revenir à la relecture"
          actions={[
            {
              label: asking === 'approve' ? 'Approuver' : 'Envoyer la demande',
              kind: 'primary',
              disabled: busy,
              onClick: () => void decide(asking),
            },
          ]}
        >
          <p>
            {asking === 'approve'
              ? 'La révision sera proposée aux éditeurs pour publication. La décision est définitive : elle ne pourra plus être relue.'
              : 'La révision sera renvoyée à son auteur avec votre commentaire ; il rédigera une version corrigée. La décision est définitive.'}
          </p>
          {comment.trim() ? (
            <>
              <p>Commentaire :</p>
              <Quote>{comment.trim()}</Quote>
            </>
          ) : (
            <p>Sans commentaire.</p>
          )}
        </Dialog>
      )}
    </section>
  );
}
