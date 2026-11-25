// Modal confirmation built on the native <dialog>: the browser traps focus,
// closes on Escape and makes the rest of the page inert. Focus goes to the
// least destructive action and returns to the element that opened it.
import { useEffect, useId, useRef, type ReactNode, type RefObject } from 'react';

export interface DialogAction {
  label: string;
  onClick: () => void;
  kind?: 'primary' | 'danger' | 'secondary';
  disabled?: boolean;
}

export function Dialog({
  title,
  children,
  actions,
  onCancel,
  cancelLabel = 'Annuler',
  initialFocus,
}: {
  title: string;
  children: ReactNode;
  actions: DialogAction[];
  onCancel: () => void;
  cancelLabel?: string;
  /** A field to focus instead of the cancel button (dialogs that ask for input). */
  initialFocus?: RefObject<HTMLElement | null>;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const titleId = useId();
  const bodyId = useId();
  useEffect(() => {
    const dialog = ref.current;
    const opener = document.activeElement as HTMLElement | null;
    if (dialog && !dialog.open) {
      if (typeof dialog.showModal === 'function') dialog.showModal();
      else dialog.setAttribute('open', '');
    }
    (initialFocus?.current ?? cancelRef.current)?.focus();
    return () => {
      if (opener && document.contains(opener)) opener.focus();
    };
    // Focus is placed once, when the dialog opens.
  }, []);
  return (
    <dialog
      ref={ref}
      className="dialog"
      aria-labelledby={titleId}
      aria-describedby={bodyId}
      onCancel={(e) => {
        e.preventDefault();
        onCancel();
      }}
    >
      <h2 id={titleId}>{title}</h2>
      <div id={bodyId} className="dialog-body">
        {children}
      </div>
      <div className="actions">
        {actions.map((a) => (
          <button key={a.label} type="button" className={a.kind ?? 'secondary'} disabled={a.disabled} onClick={a.onClick}>
            {a.label}
          </button>
        ))}
        <button ref={cancelRef} type="button" onClick={onCancel}>
          {cancelLabel}
        </button>
      </div>
    </dialog>
  );
}
