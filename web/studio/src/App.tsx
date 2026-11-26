// Studio shell: session, navigation, sign-out and the "session expired" /
// "another account signed in" states. Pages are chosen by the client route
// (router.ts). Next to "Mes leçons", the reviewer, publisher, administrator
// and error-report spaces (editorial/) are shown only to accounts the server
// lets use them (spaces.ts); the server enforces every rule regardless.
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { ACCOUNT_CHANGED, Api, isApiError, type Session } from './api/client';
import { describeError } from './api/messages';
import { noticeFrom } from './api/admin';
import { StudioContext, allowUnload, useStudio, type Studio } from './context';
import { Link } from './ui/Link';
import { BASE, hasBlockingGuard, onNavigate, setLeaveConfirmation, useRoute } from './router';
import { Dialog } from './ui/Dialog';
import { findUserRecovery, purgeRecovery, RECOVERY_MAX_AGE_MS, suspendRecovery } from './recovery';
import { LessonList } from './pages/LessonList';
import { NewLesson } from './pages/NewLesson';
import { EditorPage } from './pages/EditorPage';
import { CourseList } from './pages/CourseList';
import { NewCourse } from './pages/NewCourse';
import { CourseEditorPage } from './pages/CourseEditorPage';
import { findUserCourseRecovery, purgeCourseRecovery } from './courses/courseRecovery';
import { discoverSpaces, type Spaces } from './spaces';
import { SpaceNav, StudioHome, editorialPage } from './editorial/spacesUi';
import { isEditorialRoute } from './editorialRoutes';
import './editorial/editorial.css';
import { LanguageSwitch } from './ui/LanguageSwitch';
import { t } from './i18n';

/** The sign-in page accepts only /studio paths of plain segments as `next` (API.md). */
export function loginURL(pathname: string): string {
  const path = pathname.replace(/\/+$/, '');
  const next = /^\/studio(\/[A-Za-z0-9_-]+)*$/.test(path) ? path : BASE;
  return `/login?next=${encodeURIComponent(next)}`;
}

type Boot = { state: 'loading' } | { state: 'ready'; session: Session; spaces: Spaces } | { state: 'error'; message: string };

export function App() {
  const [problem, setProblem] = useState<'expired' | 'account' | null>(null);
  const booted = useRef(false);
  const api = useMemo(
    () =>
      new Api({
        onUnauthenticated: () => {
          // Before boot there is nothing to lose: go and sign in. Afterwards keep the page (and its edits).
          if (!booted.current) {
            allowUnload();
            location.assign(loginURL(location.pathname));
          } else setProblem((p) => p ?? 'expired');
        },
        onAccountChanged: () => setProblem('account'),
      }),
    [],
  );
  const [boot, setBoot] = useState<Boot>({ state: 'loading' });
  const [leave, setLeave] = useState<(() => void) | null>(null);

  useEffect(() => {
    setLeaveConfirmation((proceed) => setLeave(() => proceed));
    // Another account may have signed in from another tab: re-check before showing any page's data,
    // and as soon as this tab is shown again (the banner then replaces nothing on screen).
    const removeNavigate = onNavigate(() => api.invalidateAccount());
    const recheck = () => {
      if (document.visibilityState !== 'visible' || !booted.current) return;
      api.invalidateAccount();
      api.session().catch(() => {});
    };
    document.addEventListener('visibilitychange', recheck);
    window.addEventListener('focus', recheck);
    // Recovery copies are short-lived crash protection: forget old ones.
    purgeRecovery(localStorage, { olderThan: new Date(Date.now() - RECOVERY_MAX_AGE_MS) });
    purgeCourseRecovery(localStorage, { olderThan: new Date(Date.now() - RECOVERY_MAX_AGE_MS) });
    (async () => {
      try {
        const session = await api.session();
        const spaces = await discoverSpaces(api);
        booted.current = true;
        setBoot({ state: 'ready', session, spaces });
      } catch (error) {
        if (!isApiError(error, 'unauthenticated')) setBoot({ state: 'error', message: describeError(error) });
      }
    })();
    return () => {
      removeNavigate();
      document.removeEventListener('visibilitychange', recheck);
      window.removeEventListener('focus', recheck);
    };
  }, [api]);

  if (boot.state === 'loading') return <p className="loading" role="status">{t('shell.loading')}</p>;
  if (boot.state === 'error')
    return (
      <main className="page" id="main">
        <h1 tabIndex={-1}>{t('shell.unavailable')}</h1>
        <p role="alert">{boot.message}</p>
        <button type="button" onClick={() => location.reload()}>
          {t('shell.retry')}
        </button>
      </main>
    );

  const studio: Studio = { api, session: boot.session, isAuthor: boot.spaces.author, spaces: boot.spaces, expired: problem !== null, accountChanged: problem === 'account' };
  return (
    <StudioContext.Provider value={studio}>
      <Shell onRenewed={() => setProblem(null)} />
      {leave && (
        <Dialog
          title={t('shell.leaveTitle')}
          onCancel={() => setLeave(null)}
          cancelLabel={t('shell.leaveStay')}
          actions={[
            {
              label: t('shell.leaveGo'),
              kind: 'danger',
              onClick: () => {
                const proceed = leave;
                setLeave(null);
                proceed();
              },
            },
          ]}
        >
          <p>{t('shell.leaveText')}</p>
        </Dialog>
      )}
    </StudioContext.Provider>
  );
}

function Shell({ onRenewed }: { onRenewed: () => void }) {
  const studio = useStudio();
  const route = useRoute();
  const [signingOut, setSigningOut] = useState<'ask' | 'busy' | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  // A one-time notice passed in the address (api/admin.ts noticeFrom), dropped from it once read.
  const [notice] = useState(() => noticeFrom(location.search));
  useEffect(() => {
    const params = new URLSearchParams(location.search);
    if (params.has('notice')) {
      params.delete('notice');
      const rest = params.toString();
      history.replaceState(history.state, '', location.pathname + (rest ? `?${rest}` : '') + location.hash);
    }
  }, []);

  const userId = studio.session.user.id;
  const signOut = useCallback(async () => {
    setSigningOut('busy');
    try {
      await studio.api.logout();
    } catch (error) {
      // Another account's session is not ours to end; an expired one is already gone.
      if (!isApiError(error, 'unauthenticated') && !isApiError(error, ACCOUNT_CHANGED)) {
        setSigningOut(null);
        setMessage(t('shell.signOutFailed', { error: describeError(error) }));
        return;
      }
    }
    // Shared devices: this account's unsaved-edit copies do not outlive its session.
    suspendRecovery();
    purgeRecovery(localStorage, { userId });
    purgeCourseRecovery(localStorage, { userId });
    allowUnload();
    location.assign('/login');
  }, [studio.api, userId]);

  let page: ReactNode;
  if (isEditorialRoute(route)) page = editorialPage(route, studio.spaces);
  else if (!studio.isAuthor) page = route.name === 'notFound' ? <NotFound /> : <StudioHome spaces={studio.spaces} />;
  else if (route.name === 'lessons') page = <LessonList />;
  else if (route.name === 'new') page = <NewLesson />;
  else if (route.name === 'edit') page = <EditorPage key={route.lessonId} lessonId={route.lessonId} />;
  else if (route.name === 'courses') page = <CourseList />;
  else if (route.name === 'newCourse') page = <NewCourse />;
  else if (route.name === 'course') page = <CourseEditorPage key={route.courseId} courseId={route.courseId} />;
  else page = <NotFound />;

  return (
    <div className="studio">
      <a className="skip" href="#main">
        {t('shell.skip')}
      </a>
      <header className="studio-header">
        <p className="brand">
          LEARNOO <span>Studio</span>
        </p>
        <nav aria-label={t('shell.nav')}>
          {studio.isAuthor && (
            <Link to={`${BASE}/`} current={route.name === 'lessons'}>
              {t('shell.myLessons')}
            </Link>
          )}
          {studio.isAuthor && (
            <Link to={`${BASE}/courses`} current={route.name === 'courses' || route.name === 'newCourse' || route.name === 'course'}>
              {t('shell.myCourses')}
            </Link>
          )}
          <SpaceNav spaces={studio.spaces} route={route} />
        </nav>
        <div className="account">
          <LanguageSwitch />
          <bdi className="who">{studio.session.user.displayName}</bdi>
          <button
            type="button"
            onClick={() => (hasBlockingGuard() || findUserRecovery(localStorage, userId).length > 0 || findUserCourseRecovery(localStorage, userId).length > 0 ? setSigningOut('ask') : void signOut())}
            disabled={signingOut === 'busy'}
          >
            {t('shell.signOut')}
          </button>
        </div>
      </header>
      {studio.expired && <ExpiredBanner onRenewed={onRenewed} />}
      {notice && (
        <p className="banner" role="status">
          {notice}
        </p>
      )}
      {message && (
        <p className="banner error" role="alert">
          {message}
        </p>
      )}
      {page}
      {signingOut === 'ask' && (
        <Dialog
          title={t('shell.signOutTitle')}
          onCancel={() => setSigningOut(null)}
          cancelLabel={t('shell.signOutBack')}
          actions={[{ label: t('shell.signOutAnyway'), kind: 'danger', onClick: () => void signOut() }]}
        >
          <p>{t('shell.signOutText')}</p>
        </Dialog>
      )}
    </div>
  );
}

/**
 * Shown after a 401 while signed in. Nothing on the page is discarded: the
 * author signs in again in another tab, then resumes here (the session's
 * new CSRF token is read before the next write).
 */
function ExpiredBanner({ onRenewed }: { onRenewed: () => void }) {
  const studio = useStudio();
  const [error, setError] = useState<string | null>(null);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => ref.current?.focus(), []);
  const resume = async () => {
    try {
      // session() refuses another account before keeping anything from it.
      await studio.api.session();
      setError(null);
      onRenewed();
    } catch (e) {
      setError(
        isApiError(e, 'unauthenticated')
          ? t('shell.stillSignedOut')
          : isApiError(e, ACCOUNT_CHANGED)
            ? t('shell.stillOtherAccount', { email: studio.session.user.email })
            : describeError(e),
      );
    }
  };
  return (
    <div className="banner expired" role="alert" tabIndex={-1} ref={ref}>
      {studio.accountChanged ? (
        <p>
          <strong>{t('shell.otherAccountTitle')}</strong> {t('shell.otherAccountText', { email: studio.session.user.email })}
        </p>
      ) : (
        <p>
          <strong>{t('shell.expiredTitle')}</strong> {t('shell.expiredText')}
        </p>
      )}
      <p>
        <a href={loginURL(location.pathname)} target="_blank" rel="noopener">
          {t('shell.signInAgain')}
        </a>
        {t('shell.then')}{' '}
        <button type="button" onClick={() => void resume()}>
          {t('shell.resume')}
        </button>
      </p>
      {error && <p className="error">{error}</p>}
    </div>
  );
}

function NotFound() {
  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>{t('shell.notFound')}</h1>
      <p>
        <Link to={`${BASE}/`}>{t('shell.backHome')}</Link>
      </p>
    </main>
  );
}
