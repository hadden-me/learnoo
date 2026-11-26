// The labelled list of unsaved-edit recovery copies (recovery.ts), with
// restore (only where a draft is open), copy and delete.
import { useState } from 'react';
import { recoveryText } from '../drafts';
import { t } from '../i18n';
import { formatDateTime } from '../labels';
import { removeRecovery, type RecoveryEntry } from '../recovery';
import type { Lang } from '../schema/lesson';

export function RecoveryList({
  entries,
  currentRevisionId,
  language,
  onRestore,
  onChanged,
  note,
}: {
  entries: RecoveryEntry[];
  currentRevisionId: number | null;
  language: Lang;
  /** Absent when no draft is open: the copies can then be copied or deleted only. */
  onRestore?: (entry: RecoveryEntry) => void;
  onChanged: () => void;
  note?: string;
}) {
  const [copied, setCopied] = useState<string | null>(null);
  const [fallback, setFallback] = useState<string | null>(null);
  if (entries.length === 0) return null;
  const copy = async (entry: RecoveryEntry) => {
    const text = recoveryText(entry.data, language);
    try {
      await navigator.clipboard.writeText(text);
      setFallback(null);
      setCopied(entry.key);
    } catch {
      // Clipboard refused (permissions, insecure context): show the text to copy by hand.
      setFallback(text);
    }
  };
  return (
    <section className="recovery" aria-labelledby="recovery-heading">
      <h2 id="recovery-heading">{t('lessons.recovery.title')}</h2>
      <p className="hint">{t('lessons.recovery.hint')}</p>
      {note && <p>{note}</p>}
      <ul>
        {entries.map((e) => (
          <li key={e.key}>
            <p>
              {t('lessons.recovery.copyOf', { date: formatDateTime(e.savedAt) })}
              {e.revisionId !== null && e.revisionId !== currentRevisionId
                ? e.revisionNumber
                  ? t('lessons.recovery.onRevision', { number: e.revisionNumber, version: String(e.baseVersion) })
                  : t('lessons.recovery.onOtherRevision', { version: String(e.baseVersion) })
                : e.baseVersion !== null
                  ? t('lessons.recovery.fromVersion', { version: e.baseVersion })
                  : ''}
              {e.reason === 'conflict' ? t('lessons.recovery.keptInConflict') : ''}
              {t('lessons.recovery.titled', { title: e.data.title || t('lessons.untitled') })}
            </p>
            <div className="actions">
              {onRestore && (
                <button type="button" onClick={() => onRestore(e)}>
                  {t('lessons.recovery.restore')}
                </button>
              )}
              <button type="button" onClick={() => void copy(e)}>
                {t('lessons.recovery.copy')}
              </button>
              <button
                type="button"
                onClick={() => {
                  removeRecovery(localStorage, e.key);
                  onChanged();
                }}
              >
                {t('lessons.recovery.delete')}
              </button>
            </div>
            {copied === e.key && <p role="status">{t('lessons.recovery.copied')}</p>}
          </li>
        ))}
      </ul>
      {fallback !== null && (
        <div className="copy-fallback">
          <p className="hint" id="recovery-copy-hint">
            {t('lessons.clipboardRefused')}
          </p>
          <textarea readOnly dir="ltr" rows={8} value={fallback} aria-describedby="recovery-copy-hint" aria-label={t('lessons.recovery.textLabel')} onFocus={(ev) => ev.currentTarget.select()} />
          <button type="button" onClick={() => setFallback(null)}>
            {t('lessons.close')}
          </button>
        </div>
      )}
    </section>
  );
}
