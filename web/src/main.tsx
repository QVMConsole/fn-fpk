import React from 'react';
import ReactDOM from 'react-dom/client';
import '@douyinfe/semi-ui/lib/es/_base/base.css';
import './styles.css';
import App from './App';

function applyTheme(): void {
  const dark = window.matchMedia('(prefers-color-scheme: dark)').matches;
  document.body.setAttribute('theme-mode', dark ? 'dark' : 'light');
}

applyTheme();
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', applyTheme);

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
