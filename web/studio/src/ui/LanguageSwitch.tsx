// The interface language switch: a real link to the current route with
// ?lang=, which the server stores in the locale cookie before coming back
// (internal/i18n). It reloads the page, so unsaved edits get the usual
// "leave without saving?" prompt and their local recovery copy. The link is
// named in its own language (lang/dir), as the reader's is.
import { dirOfLocale, locale, switchHref, t, type Locale } from '../i18n';
import { useRoute } from '../router';

const NAMES: Record<Locale, string> = { fr: 'Français', ar: 'العربية' };

export function LanguageSwitch() {
  useRoute(); // re-render on client navigation so the link names the current route
  const target: Locale = locale() === 'ar' ? 'fr' : 'ar';
  return (
    <a className="lang-switch" href={switchHref(target)} hrefLang={target} lang={target} dir={dirOfLocale(target)} title={t('shell.languageSwitch')}>
      {NAMES[target]}
    </a>
  );
}
