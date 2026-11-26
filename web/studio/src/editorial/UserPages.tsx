// Administration of users (LMS-B10): find an account, change its separate
// grants (author, publisher, administrator, reviewer per subject) and its
// status, read its history, and invite students. Every change is made and
// audited by the server, with this administrator as actor; the server also
// refuses self-grants and self-disabling, and the last active administrator.
import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react';
import {
  GLOBAL_PERMISSIONS,
  PERMISSION_LABELS,
  describeAdminError,
  invitationLink,
  selfRules,
  type AdminSubject,
  type AdminUser,
  type GlobalPermission,
  type Invitation,
  type UserEvent,
  type UserFilter,
} from '../api/admin';
import { CatalogueName, entryLabel } from '../catalogue';
import { useStudio } from '../context';
import { paths } from '../editorialRoutes';
import { formatDateTime } from '../labels';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { AdminPage, LoadFailed, OutcomeBanner, failure, useAdmin, type Outcome } from './adminCommon';
import { latestOnly, useLoad } from './common';

type ListState = { state: 'loading' } | { state: 'error'; message: string } | { state: 'ready'; users: AdminUser[]; nextAfter: number | null; more: boolean };

const PERMISSION_FILTERS: { value: NonNullable<UserFilter['permission']>; label: string }[] = [
  { value: '', label: 'Toutes' },
  { value: 'author', label: 'Enseignants (auteur)' },
  { value: 'reviewer', label: 'Relecteurs' },
  { value: 'publisher', label: 'Éditeurs (publication)' },
  { value: 'administrator', label: 'Administrateurs' },
  { value: 'none', label: 'Élèves (aucune permission)' },
];

export function UserListPage() {
  const admin = useAdmin();
  const [q, setQ] = useState('');
  const [permission, setPermission] = useState<NonNullable<UserFilter['permission']>>('');
  const [status, setStatus] = useState<NonNullable<UserFilter['status']>>('');
  const [filter, setFilter] = useState<UserFilter>({});
  const [list, setList] = useState<ListState>({ state: 'loading' });
  const [latest] = useState(latestOnly);
  const [moreError, setMoreError] = useState<string | null>(null);

  const load = useCallback(async () => {
    const ticket = latest.next();
    setList({ state: 'loading' });
    try {
      const page = await admin.users(filter);
      if (latest.isCurrent(ticket)) setList({ state: 'ready', users: page.users, nextAfter: page.nextAfter, more: false });
    } catch (e) {
      if (latest.isCurrent(ticket)) setList({ state: 'error', message: describeAdminError(e) });
    }
  }, [admin, filter, latest]);
  useEffect(() => {
    void load();
  }, [load]);

  const more = async () => {
    if (list.state !== 'ready' || list.nextAfter === null) return;
    const ticket = latest.next();
    setMoreError(null);
    setList({ ...list, more: true });
    try {
      const page = await admin.users({ ...filter, after: list.nextAfter });
      if (latest.isCurrent(ticket)) setList({ state: 'ready', users: [...list.users, ...page.users], nextAfter: page.nextAfter, more: false });
    } catch (e) {
      if (latest.isCurrent(ticket)) {
        setList({ ...list, more: false });
        setMoreError(describeAdminError(e));
      }
    }
  };

  const search = (e: FormEvent) => {
    e.preventDefault();
    setFilter({ q: q.trim().slice(0, 100), permission, status });
  };

  return (
    <AdminPage
      title="Utilisateurs"
      intro={
        <p className="hint">
          Chaque permission est attribuée séparément. Un administrateur n’est relecteur que des matières qui lui sont attribuées. Chaque changement est enregistré dans l’historique du compte.
        </p>
      }
    >
      <form className="filters" onSubmit={search} role="search" aria-label="Rechercher des utilisateurs">
        <div className="fields">
          <label htmlFor="user-q">Rechercher (e-mail ou nom)</label>
          <input id="user-q" type="search" value={q} maxLength={100} autoComplete="off" onChange={(e) => setQ(e.target.value)} />
        </div>
        <div className="fields">
          <label htmlFor="user-permission">Permission</label>
          <select id="user-permission" value={permission} onChange={(e) => setPermission(e.target.value as typeof permission)}>
            {PERMISSION_FILTERS.map((f) => (
              <option key={f.value} value={f.value}>
                {f.label}
              </option>
            ))}
          </select>
        </div>
        <div className="fields">
          <label htmlFor="user-status">Statut</label>
          <select id="user-status" value={status} onChange={(e) => setStatus(e.target.value as typeof status)}>
            <option value="">Tous</option>
            <option value="active">Actifs</option>
            <option value="disabled">Désactivés</option>
          </select>
        </div>
        <button type="submit" className="primary">
          Rechercher
        </button>
      </form>

      {list.state === 'loading' && <p role="status">Chargement des utilisateurs…</p>}
      {list.state === 'error' && <LoadFailed message={list.message} onRetry={() => void load()} />}
      {list.state === 'ready' && list.users.length === 0 && <p>Aucun utilisateur ne correspond à cette recherche.</p>}
      {list.state === 'ready' && list.users.length > 0 && (
        <>
          <ul className="lesson-list" aria-label="Utilisateurs">
            {list.users.map((u) => (
              <li key={u.id} className="lesson-card">
                <h2>
                  <Link to={paths.adminUser(u.id)}>{u.displayName}</Link>
                </h2>
                <p className="lesson-context" dir="ltr">
                  {u.email}
                </p>
                <UserStates user={u} />
              </li>
            ))}
          </ul>
          {moreError && (
            <p className="banner error" role="alert">
              {moreError}
            </p>
          )}
          {list.nextAfter !== null && (
            <button type="button" onClick={() => void more()} disabled={list.more}>
              {list.more ? 'Chargement…' : 'Afficher plus'}
            </button>
          )}
        </>
      )}

      <InviteStudent />
    </AdminPage>
  );
}

/** Status and permissions, written out (colour never stands alone). */
function UserStates({ user }: { user: AdminUser }) {
  return (
    <p className="lesson-meta">
      {user.disabled ? <span className="state state-withdrawn">Désactivé</span> : <span className="state">Actif</span>}
      {user.permissions.map((p) => (
        <span key={p} className="state">
          {PERMISSION_LABELS[p]}
        </span>
      ))}
      {user.reviewerSubjects.length > 0 && <span className="state">Relecteur ({user.reviewerSubjects.length} matière{user.reviewerSubjects.length > 1 ? 's' : ''})</span>}
      {user.permissions.length === 0 && user.reviewerSubjects.length === 0 && <span className="state">Élève (aucune permission)</span>}
    </p>
  );
}

function InviteStudent() {
  const admin = useAdmin();
  const [email, setEmail] = useState('');
  const [hours, setHours] = useState('72');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<Invitation | null>(null);
  const linkRef = useRef<HTMLInputElement>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const ttl = Number(hours);
    if (!email.trim()) return setError('Saisissez l’adresse e-mail de l’élève.');
    if (!Number.isInteger(ttl) || ttl < 1 || ttl > 720) return setError('La validité doit être un nombre entier d’heures, de 1 à 720.');
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      setResult(await admin.invite(email.trim(), ttl));
      setEmail('');
    } catch (err) {
      setError(describeAdminError(err));
    } finally {
      setBusy(false);
    }
  };
  useEffect(() => {
    if (result) linkRef.current?.focus();
  }, [result]);

  return (
    <section aria-labelledby="invite-title">
      <h2 id="invite-title">Inviter un élève</h2>
      <p className="hint">
        L’invitation crée un compte élève sans permission. Pour un enseignant : invitez-le, puis attribuez-lui la permission auteur une fois son compte créé. Aucun e-mail n’est envoyé :
        transmettez vous-même le lien.
      </p>
      <form onSubmit={submit} noValidate>
        <div className="fields">
          <label htmlFor="invite-email">Adresse e-mail de l’élève</label>
          <input id="invite-email" type="email" dir="ltr" autoComplete="off" value={email} onChange={(e) => setEmail(e.target.value)} />
        </div>
        <div className="fields">
          <label htmlFor="invite-hours">Validité (heures, de 1 à 720)</label>
          <input id="invite-hours" type="number" min={1} max={720} inputMode="numeric" value={hours} onChange={(e) => setHours(e.target.value)} />
        </div>
        {error && (
          <p className="banner error" role="alert">
            {error}
          </p>
        )}
        <button type="submit" className="primary" disabled={busy}>
          Créer l’invitation
        </button>
      </form>
      {result && (
        <div className="banner success" role="status">
          <p>
            Invitation créée pour <span dir="ltr">{result.invitation.email}</span>, valable jusqu’au {formatDateTime(result.invitation.expiresAt)}.
          </p>
          <div className="fields">
            <label htmlFor="invite-link">Lien d’invitation</label>
            <input id="invite-link" ref={linkRef} readOnly dir="ltr" value={invitationLink(location.origin, result.path)} onFocus={(e) => e.currentTarget.select()} />
          </div>
          <p>Ce lien n’est affiché qu’une fois et n’est pas conservé : copiez-le maintenant. Aucun e-mail n’a été envoyé.</p>
        </div>
      )}
    </section>
  );
}

type Pending =
  | { kind: 'grant' | 'revoke'; permission: GlobalPermission }
  | { kind: 'revokeReviewer'; subjectId: number; label: string }
  | { kind: 'disable' }
  | { kind: 'enable' };

/** Changes asked to be confirmed: revocations, the administrator grant, and status changes. */
export const needsConfirmation = (p: Pending) => p.kind !== 'grant' || p.permission === 'administrator';

const pendingTitle = (p: Pending, name: string): string => {
  switch (p.kind) {
    case 'grant':
      return `Attribuer la permission ${PERMISSION_LABELS[p.permission]} ?`;
    case 'revoke':
      return `Retirer la permission ${PERMISSION_LABELS[p.permission]} ?`;
    case 'revokeReviewer':
      return 'Retirer cette matière de relecture ?';
    case 'disable':
      return `Désactiver le compte de ${name} ?`;
    case 'enable':
      return `Réactiver le compte de ${name} ?`;
  }
};

export function UserPage({ userId }: { userId: number }) {
  const admin = useAdmin();
  const { session } = useStudio();
  const [loaded, reload] = useLoad(() => admin.user(userId), [admin, userId]);
  const [catalogue] = useLoad(() => admin.catalogue(), [admin]);
  const [pending, setPending] = useState<Pending | null>(null);
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome>(null);
  const [subjectId, setSubjectId] = useState('');

  if (loaded.state === 'loading')
    return (
      <AdminPage title="Utilisateur">
        <p role="status">Chargement du compte…</p>
      </AdminPage>
    );
  if (loaded.state === 'error')
    return (
      <AdminPage title="Utilisateur">
        <LoadFailed message={loaded.message} onRetry={() => void reload()} />
        <p>
          <Link to={paths.adminUsers}>Revenir aux utilisateurs</Link>
        </p>
      </AdminPage>
    );

  const { user, history } = loaded.data;
  const isSelf = user.id === session.user.id;
  const rules = selfRules(isSelf);

  const run = async (p: Pending) => {
    setBusy(true);
    setOutcome(null);
    try {
      let res: { changed: boolean };
      let done: string;
      switch (p.kind) {
        case 'grant':
          res = await admin.grant(user.id, p.permission);
          done = `Permission ${PERMISSION_LABELS[p.permission]} attribuée.`;
          break;
        case 'revoke':
          res = await admin.revoke(user.id, p.permission);
          done = `Permission ${PERMISSION_LABELS[p.permission]} retirée.`;
          break;
        case 'revokeReviewer':
          res = await admin.revoke(user.id, 'reviewer', p.subjectId);
          done = `Relecture retirée pour ${p.label}.`;
          break;
        case 'disable':
          res = await admin.disable(user.id);
          done = 'Compte désactivé : ses sessions sont fermées.';
          break;
        case 'enable':
          res = await admin.enable(user.id);
          done = 'Compte réactivé.';
          break;
      }
      setOutcome({ kind: 'success', message: res.changed ? done : 'Aucun changement : c’était déjà le cas.' });
      await reload();
    } catch (e) {
      setOutcome(failure(e));
    } finally {
      setBusy(false);
      setPending(null);
    }
  };
  const ask = (p: Pending) => (needsConfirmation(p) ? setPending(p) : void run(p));

  const grantReviewer = async (e: FormEvent) => {
    e.preventDefault();
    const id = Number(subjectId);
    if (!id) return setOutcome({ kind: 'error', message: 'Choisissez une matière.' });
    setBusy(true);
    setOutcome(null);
    try {
      const res = await admin.grant(user.id, 'reviewer', id);
      setOutcome({ kind: 'success', message: res.changed ? 'Matière de relecture attribuée.' : 'Aucun changement : ce compte relit déjà cette matière.' });
      setSubjectId('');
      await reload();
    } catch (err) {
      setOutcome(failure(err));
    } finally {
      setBusy(false);
    }
  };

  const held = new Set(user.reviewerSubjects.map((s) => s.id));
  const assignable: AdminSubject[] = catalogue.state === 'ready' ? catalogue.data.filter((s) => s.retiredAt === null && !held.has(s.id)) : [];

  return (
    <AdminPage title={user.displayName}>
      <p className="breadcrumb">
        <Link to={paths.adminUsers}>Utilisateurs</Link>
      </p>
      <dl className="facts">
        <div>
          <dt>Adresse e-mail</dt>
          <dd dir="ltr">{user.email}</dd>
        </div>
        <div>
          <dt>Statut</dt>
          <dd>{user.disabled ? 'Désactivé' : 'Actif'}</dd>
        </div>
        <div>
          <dt>Créé le</dt>
          <dd>{formatDateTime(user.createdAt)}</dd>
        </div>
      </dl>
      {isSelf && (
        <p className="banner">
          C’est votre propre compte : le serveur refuse que vous vous attribuiez une permission ou que vous désactiviez ce compte. Vous pouvez retirer vos propres permissions (la
          permission administrateur seulement s’il reste un autre administrateur actif).
        </p>
      )}
      <OutcomeBanner outcome={outcome} />

      <section aria-labelledby="status-title">
        <h2 id="status-title">Statut du compte</h2>
        {user.disabled ? (
          <>
            <p>Ce compte est désactivé : il ne peut pas se connecter et n’a aucune permission effective. Ses permissions restent enregistrées et s’appliquent de nouveau à la réactivation.</p>
            <button type="button" disabled={busy} onClick={() => ask({ kind: 'enable' })}>
              Réactiver le compte
            </button>
          </>
        ) : (
          <>
            <p>Ce compte est actif.</p>
            {rules.canDisable && (
              <button type="button" className="danger" disabled={busy} onClick={() => ask({ kind: 'disable' })}>
                Désactiver le compte
              </button>
            )}
          </>
        )}
      </section>

      <section aria-labelledby="perm-title">
        <h2 id="perm-title">Permissions</h2>
        <ul className="grant-list">
          {GLOBAL_PERMISSIONS.map((p) => {
            const has = user.permissions.includes(p);
            return (
              <li key={p}>
                <span>
                  <strong>{PERMISSION_LABELS[p]}</strong> : {has ? 'attribuée' : 'non attribuée'}
                </span>
                {has && rules.canRevoke && (
                  <button type="button" className="danger" disabled={busy} onClick={() => ask({ kind: 'revoke', permission: p })}>
                    Retirer {PERMISSION_LABELS[p]}
                  </button>
                )}
                {!has && rules.canGrant && (
                  <button type="button" disabled={busy} onClick={() => ask({ kind: 'grant', permission: p })}>
                    Attribuer {PERMISSION_LABELS[p]}
                  </button>
                )}
              </li>
            );
          })}
        </ul>
      </section>

      <section aria-labelledby="review-title">
        <h2 id="review-title">Matières de relecture</h2>
        <p className="hint">La permission administrateur ne donne pas le droit de relire : seul un compte à qui une matière est attribuée ici peut relire les leçons de cette matière.</p>
        {user.reviewerSubjects.length === 0 ? (
          <p>Aucune matière de relecture.</p>
        ) : (
          <ul className="grant-list" aria-label="Matières de relecture">
            {user.reviewerSubjects.map((s) => {
              const label = entryLabel({ ...s, sortOrder: 0 }, 'fr');
              return (
                <li key={s.id}>
                  <span>
                    <CatalogueName label={label} /> {s.retiredAt && '(retirée)'}
                  </span>
                  <button type="button" className="danger" disabled={busy} onClick={() => ask({ kind: 'revokeReviewer', subjectId: s.id, label: label.text })}>
                    Retirer la relecture de {label.text}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
        {rules.canGrant && (
          <form className="find-lesson" onSubmit={grantReviewer}>
            <div className="fields">
              <label htmlFor="reviewer-subject">Ajouter une matière de relecture</label>
              {catalogue.state === 'error' && (
                <p className="field-error" role="alert">
                  Liste des matières indisponible. {catalogue.message}
                </p>
              )}
              <select id="reviewer-subject" value={subjectId} onChange={(e) => setSubjectId(e.target.value)} disabled={catalogue.state !== 'ready'}>
                <option value="">{catalogue.state === 'loading' ? 'Chargement des matières…' : assignable.length ? 'Choisir une matière' : 'Aucune matière disponible'}</option>
                {assignable.map((s) => {
                  const label = entryLabel(s, 'fr');
                  return (
                    <option key={s.id} value={s.id} lang={label.lang === 'ar' ? 'ar' : undefined}>
                      {label.text}
                    </option>
                  );
                })}
              </select>
            </div>
            <button type="submit" disabled={busy || !subjectId}>
              Attribuer la relecture
            </button>
          </form>
        )}
      </section>

      <section aria-labelledby="history-title">
        <h2 id="history-title">Historique</h2>
        {history.length === 0 ? <p>Aucun changement enregistré.</p> : <HistoryList history={history} />}
      </section>

      {pending && (
        <Dialog
          title={pendingTitle(pending, user.displayName)}
          onCancel={() => setPending(null)}
          actions={[
            {
              label: confirmLabel(pending),
              kind: pending.kind === 'enable' || pending.kind === 'grant' ? 'primary' : 'danger',
              disabled: busy,
              onClick: () => void run(pending),
            },
          ]}
        >
          <p>{confirmText(pending)}</p>
        </Dialog>
      )}
    </AdminPage>
  );
}

function confirmLabel(p: Pending): string {
  switch (p.kind) {
    case 'grant':
      return 'Attribuer';
    case 'revoke':
    case 'revokeReviewer':
      return 'Retirer';
    case 'disable':
      return 'Désactiver';
    case 'enable':
      return 'Réactiver';
  }
}

function confirmText(p: Pending): string {
  switch (p.kind) {
    case 'grant':
      return 'Un administrateur peut gérer les utilisateurs, leurs permissions, le catalogue et les programmes. Il ne devient pas relecteur pour autant.';
    case 'revoke':
      return p.permission === 'administrator'
        ? 'Le compte ne pourra plus administrer. Le serveur refuse si c’est le dernier administrateur actif.'
        : 'Le changement s’applique dès la prochaine requête du compte.';
    case 'revokeReviewer':
      return `Le compte ne pourra plus relire les leçons de ${p.label}.`;
    case 'disable':
      return 'Le compte ne pourra plus se connecter et ses sessions ouvertes sont fermées immédiatement. Ses permissions restent enregistrées. Le serveur refuse s’il s’agit du dernier administrateur actif.';
    case 'enable':
      return 'Le compte pourra de nouveau se connecter avec ses permissions enregistrées.';
  }
}

const ACTION_LABELS: Record<UserEvent['action'], string> = { grant: 'Attribution', revoke: 'Retrait', disable: 'Désactivation', enable: 'Réactivation' };

/** One history event as a French sentence. */
export function describeUserEvent(e: UserEvent): string {
  const who = e.actorId !== null ? (e.actorName ?? `utilisateur n° ${e.actorId}`) : `opérateur ${e.operatorLabel ?? 'inconnu'}`;
  const what =
    e.kind === 'status'
      ? ACTION_LABELS[e.action]
      : `${ACTION_LABELS[e.action]} : ${e.permission ? PERMISSION_LABELS[e.permission] : ''}${e.subjectCode ? ` (${e.subjectCode})` : ''}`;
  return `${what}, par ${who}`;
}

function HistoryList({ history }: { history: UserEvent[] }) {
  return (
    <ul className="history">
      {history.map((e, i) => (
        <li key={i}>
          <p>{describeUserEvent(e)}</p>
          <p className="lesson-context">{formatDateTime(e.occurredAt)}</p>
        </li>
      ))}
    </ul>
  );
}
