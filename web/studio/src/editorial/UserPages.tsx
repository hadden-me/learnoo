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
  adminRevokedHome,
  leavesAdministration,
  selfRules,
  type AdminSubject,
  type AdminUser,
  type GlobalPermission,
  type Invitation,
  type UserEvent,
  type UserFilter,
} from '../api/admin';
import { CatalogueName, entryLabel } from '../catalogue';
import { allowUnload, useStudio } from '../context';
import { paths } from '../editorialRoutes';
import { locale, t, tn } from '../i18n';
import { formatDateTime } from '../labels';
import { Dialog } from '../ui/Dialog';
import { Link } from '../ui/Link';
import { AdminPage, LoadFailed, OutcomeBanner, failure, useAdmin, type Outcome } from './adminCommon';
import { latestOnly, useLoad } from './common';

type ListState = { state: 'loading' } | { state: 'error'; message: string } | { state: 'ready'; users: AdminUser[]; nextAfter: number | null; more: boolean };

const PERMISSION_FILTERS: { value: NonNullable<UserFilter['permission']>; label: string }[] = [
  { value: '', label: t('admin.users.filter.all') },
  { value: 'author', label: t('admin.users.filter.author') },
  { value: 'reviewer', label: t('admin.users.filter.reviewer') },
  { value: 'publisher', label: t('admin.users.filter.publisher') },
  { value: 'administrator', label: t('admin.users.filter.administrator') },
  { value: 'none', label: t('admin.users.filter.none') },
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
      title={t('admin.nav.users')}
      intro={<p className="hint">{t('admin.users.intro')}</p>}
    >
      <form className="filters" onSubmit={search} role="search" aria-label={t('admin.users.search')}>
        <div className="fields">
          <label htmlFor="user-q">{t('admin.users.query')}</label>
          <input id="user-q" type="search" value={q} maxLength={100} autoComplete="off" onChange={(e) => setQ(e.target.value)} />
        </div>
        <div className="fields">
          <label htmlFor="user-permission">{t('admin.users.permission')}</label>
          <select id="user-permission" value={permission} onChange={(e) => setPermission(e.target.value as typeof permission)}>
            {PERMISSION_FILTERS.map((f) => (
              <option key={f.value} value={f.value}>
                {f.label}
              </option>
            ))}
          </select>
        </div>
        <div className="fields">
          <label htmlFor="user-status">{t('admin.status')}</label>
          <select id="user-status" value={status} onChange={(e) => setStatus(e.target.value as typeof status)}>
            <option value="">{t('admin.users.status.all')}</option>
            <option value="active">{t('admin.users.status.active')}</option>
            <option value="disabled">{t('admin.users.status.disabled')}</option>
          </select>
        </div>
        <button type="submit" className="primary">
          {t('admin.users.submit')}
        </button>
      </form>

      {list.state === 'loading' && <p role="status">{t('admin.users.loading')}</p>}
      {list.state === 'error' && <LoadFailed message={list.message} onRetry={() => void load()} />}
      {list.state === 'ready' && list.users.length === 0 && <p>{t('admin.users.empty')}</p>}
      {list.state === 'ready' && list.users.length > 0 && (
        <>
          <ul className="lesson-list" aria-label={t('admin.nav.users')}>
            {list.users.map((u) => (
              <li key={u.id} className="lesson-card">
                <h2>
                  <Link to={paths.adminUser(u.id)}>
                    <bdi>{u.displayName}</bdi>
                  </Link>
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
              {list.more ? t('shell.loadingPage') : t('admin.users.more')}
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
      {user.disabled ? <span className="state state-withdrawn">{t('admin.disabled')}</span> : <span className="state">{t('admin.active')}</span>}
      {user.permissions.map((p) => (
        <span key={p} className="state">
          {PERMISSION_LABELS[p]}
        </span>
      ))}
      {user.reviewerSubjects.length > 0 && <span className="state">{tn('admin.users.reviewerOf', user.reviewerSubjects.length)}</span>}
      {user.permissions.length === 0 && user.reviewerSubjects.length === 0 && <span className="state">{t('admin.users.student')}</span>}
    </p>
  );
}

function InviteStudent() {
  const admin = useAdmin();
  const [email, setEmail] = useState('');
  const [hours, setHours] = useState('72');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The field the error is about (validation), else none (server refusal).
  const [errorField, setErrorField] = useState<'email' | 'hours' | null>(null);
  const [result, setResult] = useState<Invitation | null>(null);
  const linkRef = useRef<HTMLInputElement>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const ttl = Number(hours);
    if (!email.trim()) {
      setErrorField('email');
      return setError(t('admin.invite.emailRequired'));
    }
    if (!Number.isInteger(ttl) || ttl < 1 || ttl > 720) {
      setErrorField('hours');
      return setError(t('admin.invite.hoursInvalid'));
    }
    setBusy(true);
    setError(null);
    setErrorField(null);
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
      <h2 id="invite-title">{t('admin.invite.title')}</h2>
      <p className="hint">{t('admin.invite.intro')}</p>
      <form onSubmit={submit} noValidate>
        <div className="fields">
          <label htmlFor="invite-email">{t('admin.invite.email')}</label>
          <input
            id="invite-email"
            type="email"
            dir="ltr"
            autoComplete="off"
            value={email}
            aria-invalid={errorField === 'email' ? true : undefined}
            aria-describedby={errorField === 'email' ? 'invite-error' : undefined}
            onChange={(e) => setEmail(e.target.value)}
          />
        </div>
        <div className="fields">
          <label htmlFor="invite-hours">{t('admin.invite.hours')}</label>
          <input
            id="invite-hours"
            type="number"
            min={1}
            max={720}
            inputMode="numeric"
            value={hours}
            aria-invalid={errorField === 'hours' ? true : undefined}
            aria-describedby={errorField === 'hours' ? 'invite-error' : undefined}
            onChange={(e) => setHours(e.target.value)}
          />
        </div>
        {error && (
          <p className="banner error" role="alert" id="invite-error">
            {error}
          </p>
        )}
        <button type="submit" className="primary" disabled={busy}>
          {t('admin.invite.submit')}
        </button>
      </form>
      {result && (
        <div className="banner success" role="status">
          <p>
            {t('admin.invite.createdFor')} <span dir="ltr">{result.invitation.email}</span>
            {t('admin.invite.validUntil', { date: formatDateTime(result.invitation.expiresAt) })}
          </p>
          <div className="fields">
            <label htmlFor="invite-link">{t('admin.invite.link')}</label>
            <input id="invite-link" ref={linkRef} readOnly dir="ltr" value={invitationLink(location.origin, result.path)} onFocus={(e) => e.currentTarget.select()} />
          </div>
          <p>{t('admin.invite.once')}</p>
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
      return t('admin.user.confirmGrant', { permission: PERMISSION_LABELS[p.permission] });
    case 'revoke':
      return t('admin.user.confirmRevoke', { permission: PERMISSION_LABELS[p.permission] });
    case 'revokeReviewer':
      return t('admin.user.confirmRevokeReviewer');
    case 'disable':
      return t('admin.user.confirmDisable', { name });
    case 'enable':
      return t('admin.user.confirmEnable', { name });
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
      <AdminPage title={t('admin.user.title')}>
        <p role="status">{t('admin.user.loading')}</p>
      </AdminPage>
    );
  if (loaded.state === 'error')
    return (
      <AdminPage title={t('admin.user.title')}>
        <LoadFailed message={loaded.message} onRetry={() => void reload()} />
        <p>
          <Link to={paths.adminUsers}>{t('admin.user.back')}</Link>
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
          done = t('admin.user.granted', { permission: PERMISSION_LABELS[p.permission] });
          break;
        case 'revoke':
          res = await admin.revoke(user.id, p.permission);
          done = t('admin.user.revoked', { permission: PERMISSION_LABELS[p.permission] });
          break;
        case 'revokeReviewer':
          res = await admin.revoke(user.id, 'reviewer', p.subjectId);
          done = t('admin.user.reviewerRevoked', { subject: p.label });
          break;
        case 'disable':
          res = await admin.disable(user.id);
          done = t('admin.user.disabledDone');
          break;
        case 'enable':
          res = await admin.enable(user.id);
          done = t('admin.user.enabledDone');
          break;
      }
      if (res.changed && leavesAdministration(isSelf, p)) {
        // No reload: this account can no longer read administration pages.
        allowUnload();
        location.assign(adminRevokedHome);
        return;
      }
      setOutcome({ kind: 'success', message: res.changed ? done : t('admin.user.unchanged') });
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
    if (!id) return setOutcome({ kind: 'error', message: t('admin.chooseSubject') });
    setBusy(true);
    setOutcome(null);
    try {
      const res = await admin.grant(user.id, 'reviewer', id);
      setOutcome({ kind: 'success', message: res.changed ? t('admin.user.reviewerGranted') : t('admin.user.reviewerUnchanged') });
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
        <Link to={paths.adminUsers}>{t('admin.nav.users')}</Link>
      </p>
      <dl className="facts">
        <div>
          <dt>{t('admin.user.email')}</dt>
          <dd dir="ltr">{user.email}</dd>
        </div>
        <div>
          <dt>{t('admin.status')}</dt>
          <dd>{user.disabled ? t('admin.disabled') : t('admin.active')}</dd>
        </div>
        <div>
          <dt>{t('admin.user.createdOn')}</dt>
          <dd>{formatDateTime(user.createdAt)}</dd>
        </div>
      </dl>
      {isSelf && (
        <p className="banner">{t('admin.user.self')}</p>
      )}
      <OutcomeBanner outcome={outcome} />

      <section aria-labelledby="status-title">
        <h2 id="status-title">{t('admin.user.statusTitle')}</h2>
        {user.disabled ? (
          <>
            <p>{t('admin.user.disabledText')}</p>
            <button type="button" disabled={busy} onClick={() => ask({ kind: 'enable' })}>
              {t('admin.user.enable')}
            </button>
          </>
        ) : (
          <>
            <p>{t('admin.user.activeText')}</p>
            {rules.canDisable && (
              <button type="button" className="danger" disabled={busy} onClick={() => ask({ kind: 'disable' })}>
                {t('admin.user.disable')}
              </button>
            )}
          </>
        )}
      </section>

      <section aria-labelledby="perm-title">
        <h2 id="perm-title">{t('admin.user.permissions')}</h2>
        <ul className="grant-list">
          {GLOBAL_PERMISSIONS.map((p) => {
            const has = user.permissions.includes(p);
            return (
              <li key={p}>
                <span>
                  <strong>{PERMISSION_LABELS[p]}</strong>
                  {t('admin.colon')}
                  {has ? t('admin.user.has') : t('admin.user.hasNot')}
                </span>
                {has && rules.canRevoke && (
                  <button type="button" className="danger" disabled={busy} onClick={() => ask({ kind: 'revoke', permission: p })}>
                    {t('admin.user.revokePermission', { permission: PERMISSION_LABELS[p] })}
                  </button>
                )}
                {!has && rules.canGrant && (
                  <button type="button" disabled={busy} onClick={() => ask({ kind: 'grant', permission: p })}>
                    {t('admin.user.grantPermission', { permission: PERMISSION_LABELS[p] })}
                  </button>
                )}
              </li>
            );
          })}
        </ul>
      </section>

      <section aria-labelledby="review-title">
        <h2 id="review-title">{t('admin.user.reviewSubjects')}</h2>
        <p className="hint">{t('admin.user.reviewHint')}</p>
        {user.reviewerSubjects.length === 0 ? (
          <p>{t('admin.user.noReviewSubject')}</p>
        ) : (
          <ul className="grant-list" aria-label={t('admin.user.reviewSubjects')}>
            {user.reviewerSubjects.map((s) => {
              const label = entryLabel({ ...s, sortOrder: 0 }, locale());
              return (
                <li key={s.id}>
                  <span>
                    <CatalogueName label={label} /> {s.retiredAt && t('admin.user.retiredSubject')}
                  </span>
                  <button type="button" className="danger" disabled={busy} onClick={() => ask({ kind: 'revokeReviewer', subjectId: s.id, label: label.text })}>
                    {t('admin.user.revokeReviewer', { subject: label.text })}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
        {rules.canGrant && (
          <form className="find-lesson" onSubmit={grantReviewer}>
            <div className="fields">
              <label htmlFor="reviewer-subject">{t('admin.user.addReviewSubject')}</label>
              {catalogue.state === 'error' && (
                <p className="field-error" role="alert" id="reviewer-subject-error">
                  {t('admin.user.subjectsUnavailable', { message: catalogue.message })}
                </p>
              )}
              <select
                id="reviewer-subject"
                value={subjectId}
                onChange={(e) => setSubjectId(e.target.value)}
                disabled={catalogue.state !== 'ready'}
                aria-describedby={catalogue.state === 'error' ? 'reviewer-subject-error' : undefined}
              >
                <option value="">{catalogue.state === 'loading' ? t('admin.loadingSubjects') : assignable.length ? t('admin.chooseSubjectOption') : t('admin.noSubjectAvailable')}</option>
                {assignable.map((s) => {
                  const label = entryLabel(s, locale());
                  return (
                    <option key={s.id} value={s.id} lang={label.lang && label.lang !== locale() ? label.lang : undefined}>
                      {label.text}
                    </option>
                  );
                })}
              </select>
            </div>
            <button type="submit" disabled={busy || !subjectId}>
              {t('admin.user.grantReviewer')}
            </button>
          </form>
        )}
      </section>

      <section aria-labelledby="history-title">
        <h2 id="history-title">{t('admin.history')}</h2>
        {history.length === 0 ? <p>{t('admin.user.noHistory')}</p> : <HistoryList history={history} />}
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
      return t('admin.user.grant');
    case 'revoke':
    case 'revokeReviewer':
      return t('admin.user.revoke');
    case 'disable':
      return t('admin.user.disableAction');
    case 'enable':
      return t('admin.user.enableAction');
  }
}

function confirmText(p: Pending): string {
  switch (p.kind) {
    case 'grant':
      return t('admin.user.grantAdminText');
    case 'revoke':
      return p.permission === 'administrator' ? t('admin.user.revokeAdminText') : t('admin.user.revokeText');
    case 'revokeReviewer':
      return t('admin.user.revokeReviewerText', { subject: p.label });
    case 'disable':
      return t('admin.user.disableText');
    case 'enable':
      return t('admin.user.enableText');
  }
}

const ACTION_LABELS: Record<UserEvent['action'], string> = {
  grant: t('admin.event.grant'),
  revoke: t('admin.event.revoke'),
  disable: t('admin.event.disable'),
  enable: t('admin.event.enable'),
};

/** One history event as a sentence in the interface language. */
export function describeUserEvent(e: UserEvent): string {
  const who = e.actorId !== null ? (e.actorName ?? t('admin.event.userId', { id: e.actorId })) : t('admin.event.operator', { label: e.operatorLabel ?? t('admin.event.unknown') });
  const what =
    e.kind === 'status'
      ? ACTION_LABELS[e.action]
      : `${t('admin.event.permission', { action: ACTION_LABELS[e.action], permission: e.permission ? PERMISSION_LABELS[e.permission] : '' })}${e.subjectCode ? ` (${e.subjectCode})` : ''}`;
  return t('admin.event.by', { what, who });
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
