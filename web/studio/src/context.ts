import { createContext, useContext } from 'react';
import type { Api, Session } from './api/client';

export interface Studio {
  api: Api;
  session: Session;
  /** False when the server refused the author listing (403): the account cannot author. */
  isAuthor: boolean;
  /** True after a 401 while signed in: edits stay on screen until the author signs in again. */
  expired: boolean;
}

export const StudioContext = createContext<Studio | null>(null);

export function useStudio(): Studio {
  const studio = useContext(StudioContext);
  if (!studio) throw new Error('StudioContext missing');
  return studio;
}

/** One-shot message shown by the next page (for example after creating a lesson). */
let flash: string | null = null;
export const setFlash = (message: string) => {
  flash = message;
};
export const takeFlash = () => {
  const message = flash;
  flash = null;
  return message;
};

/** Set once the author confirmed leaving (sign-out): the unsaved-changes prompt is then skipped. */
let unloadAllowed = false;
export const allowUnload = () => {
  unloadAllowed = true;
};
export const isUnloadAllowed = () => unloadAllowed;

/** Set by the save that created a lesson, so the editor reopened at the lesson's address gets the focus back. */
let editorFocus = false;
export const requestEditorFocus = () => {
  editorFocus = true;
};
export const takeEditorFocus = () => {
  const value = editorFocus;
  editorFocus = false;
  return value;
};
