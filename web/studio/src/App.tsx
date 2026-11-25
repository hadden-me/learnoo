// Studio shell: session, navigation, sign-out and the "session expired" /
// "another account signed in" states. Pages are chosen by the client route (router.ts). The next slice
// adds reviewer, publisher and administrator spaces next to "Mes leçons",
// each shown only to accounts the server lets use it.
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { ACCOUNT_CHANGED, Api, isApiError, type Session } from './api/client';
import { describeError } from './api/messages';
import { StudioContext, allowUnload, useStudio, type Studio } from './context';
import { Link } from './ui/Link';
import { BASE, hasBlockingGuard, setLeaveConfirmation, useRoute } from './router';
import { Dialog } from './ui/Dialog';
import { findUserRecovery, purgeRecovery, RECOVERY_MAX_AGE_MS, suspendRecovery } from './recovery';
import { LessonList } from './pages/LessonList';
import { NewLesson } from './pages/NewLesson';
import { EditorPage } from './pages/EditorPage';

/** The sign-in page accepts only /studio paths of plain segments as `next` (API.md). */
export function loginURL(pathname: string): string {
  const path = pathname.replace(/\/+$/, '');
  const next = /^\/studio(\/[A-Za-z0-9_-]+)*$/.test(path) ? path : BASE;
  return `/login?next=${encodeURIComponent(next)}`;
}

type Boot = { state: 'loading' } | { state: 'ready'; session: Session; isAuthor: boolean } | { state: 'error'; message: string };

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
    // Recovery copies are short-lived crash protection: forget old ones.
    purgeRecovery(localStorage, { olderThan: new Date(Date.now() - RECOVERY_MAX_AGE_MS) });
    (async () => {
      try {
        const session = await api.session();
        let isAuthor = true;
        try {
          await api.myLessons();
        } catch (error) {
          if (isApiError(error, 'forbidden')) isAuthor = false;
          else throw error;
        }
        booted.current = true;
        setBoot({ state: 'ready', session, isAuthor });
      } catch (error) {
        if (!isApiError(error, 'unauthenticated')) setBoot({ state: 'error', message: describeError(error) });
      }
    })();
  }, [api]);

  if (boot.state === 'loading') return <p className="loading" role="status">Chargement du Studio…</p>;
  if (boot.state === 'error')
    return (
      <main className="page" id="main">
        <h1 tabIndex={-1}>Studio indisponible</h1>
        <p role="alert">{boot.message}</p>
        <button type="button" onClick={() => location.reload()}>
          Réessayer
        </button>
      </main>
    );

  const studio: Studio = { api, session: boot.session, isAuthor: boot.isAuthor, expired: problem !== null, accountChanged: problem === 'account' };
  return (
    <StudioContext.Provider value={studio}>
      <Shell onRenewed={() => setProblem(null)} />
      {leave && (
        <Dialog
          title="Quitter sans enregistrer ?"
          onCancel={() => setLeave(null)}
          cancelLabel="Rester sur la page"
          actions={[
            {
              label: 'Quitter la page',
              kind: 'danger',
              onClick: () => {
                const proceed = leave;
                setLeave(null);
                proceed();
              },
            },
          ]}
        >
          <p>Des modifications ne sont pas enregistrées sur le serveur. Une copie de secours reste sur cet appareil et vous sera proposée à la prochaine ouverture de la leçon.</p>
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

  const userId = studio.session.user.id;
  const signOut = useCallback(async () => {
    setSigningOut('busy');
    try {
      await studio.api.logout();
    } catch (error) {
      // Another account's session is not ours to end; an expired one is already gone.
      if (!isApiError(error, 'unauthenticated') && !isApiError(error, ACCOUNT_CHANGED)) {
        setSigningOut(null);
        setMessage(`Déconnexion impossible : ${describeError(error)}`);
        return;
      }
    }
    // Shared devices: this account's unsaved-edit copies do not outlive its session.
    suspendRecovery();
    purgeRecovery(localStorage, { userId });
    allowUnload();
    location.assign('/login');
  }, [studio.api, userId]);

  let page: ReactNode;
  if (!studio.isAuthor) page = <NoAuthoring />;
  else if (route.name === 'lessons') page = <LessonList />;
  else if (route.name === 'new') page = <NewLesson />;
  else if (route.name === 'edit') page = <EditorPage key={route.lessonId} lessonId={route.lessonId} />;
  else
    page = (
      <main className="page" id="main">
        <h1 tabIndex={-1}>Page introuvable</h1>
        <p>
          <Link to={`${BASE}/`}>Revenir à mes leçons</Link>
        </p>
      </main>
    );

  return (
    <div className="studio">
      <a className="skip" href="#main">
        Aller au contenu
      </a>
      <header className="studio-header">
        <p className="brand">
          LEARNOO <span>Studio</span>
        </p>
        <nav aria-label="Studio">
          {studio.isAuthor && (
            <Link to={`${BASE}/`} current={route.name === 'lessons'}>
              Mes leçons
            </Link>
          )}
        </nav>
        <div className="account">
          <span className="who">{studio.session.user.displayName}</span>
          <button
            type="button"
            onClick={() => (hasBlockingGuard() || findUserRecovery(localStorage, userId).length > 0 ? setSigningOut('ask') : void signOut())}
            disabled={signingOut === 'busy'}
          >
            Se déconnecter
          </button>
        </div>
      </header>
      {studio.expired && <ExpiredBanner onRenewed={onRenewed} />}
      {message && (
        <p className="banner error" role="alert">
          {message}
        </p>
      )}
      {page}
      {signingOut === 'ask' && (
        <Dialog
          title="Se déconnecter sans enregistrer ?"
          onCancel={() => setSigningOut(null)}
          cancelLabel="Revenir à l’édition"
          actions={[{ label: 'Se déconnecter quand même', kind: 'danger', onClick: () => void signOut() }]}
        >
          <p>
            Des modifications ne sont pas enregistrées sur le serveur (dans cette page ou dans des copies de secours de cet appareil). Enregistrez-les d’abord : à la déconnexion, les copies de secours de votre compte sont effacées de cet appareil et ces modifications seront perdues.
          </p>
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
          ? 'Toujours pas connecté. Connectez-vous dans l’autre onglet, puis réessayez.'
          : isApiError(e, ACCOUNT_CHANGED)
            ? `Un autre compte est toujours connecté dans ce navigateur. Déconnectez-le dans l’autre onglet et reconnectez-vous avec ${studio.session.user.email}, puis réessayez.`
            : describeError(e),
      );
    }
  };
  return (
    <div className="banner expired" role="alert" tabIndex={-1} ref={ref}>
      {studio.accountChanged ? (
        <p>
          <strong>Un autre compte est connecté dans ce navigateur.</strong> Pour protéger vos leçons, cette page n’envoie plus rien au serveur. Vos modifications sont conservées sur cette page et dans la copie de secours de cet appareil. Reconnectez-vous avec {studio.session.user.email}, puis reprenez ici.
        </p>
      ) : (
        <p>
          <strong>Session expirée.</strong> Vos modifications sont conservées sur cette page et dans la copie de secours de cet appareil, mais rien ne peut être enregistré tant que vous n’êtes pas reconnecté.
        </p>
      )}
      <p>
        <a href={loginURL(location.pathname)} target="_blank" rel="noopener">
          Se reconnecter dans un nouvel onglet
        </a>
        , puis{' '}
        <button type="button" onClick={() => void resume()}>
          Reprendre ici
        </button>
      </p>
      {error && <p className="error">{error}</p>}
    </div>
  );
}

function NoAuthoring() {
  return (
    <main className="page" id="main">
      <h1 tabIndex={-1}>Studio</h1>
      <p>Votre compte n’a pas la permission auteur : vous ne pouvez pas rédiger de leçons.</p>
      <p className="hint">Les espaces de relecture et de publication seront ajoutés à cette page dans une prochaine version.</p>
    </main>
  );
}
