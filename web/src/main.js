import { mount } from 'svelte';
import './app.css';
import App from './App.svelte';

try {
  const theme = localStorage.getItem('theme');
  if (theme && theme !== 'auto') document.documentElement.dataset.theme = theme;
} catch {
  // Storage can be blocked; the system theme still applies.
}

mount(App, { target: document.getElementById('app') });
