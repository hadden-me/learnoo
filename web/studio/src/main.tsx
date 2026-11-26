import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '../../design/tokens.css';
import 'virtual:reader-lesson.css';
import './styles.css';
import { App } from './App';
import { applyToDocument } from './i18n';

// The server already set <html lang dir> from the locale cookie; the development server did not.
applyToDocument();

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
