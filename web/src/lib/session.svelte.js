// Reactive session state using Svelte 5 runes.
import { getToken, setToken } from './api.js';

export const session = $state({
  authenticated: !!getToken(),
  username: '',
  role: '',
});

export function login(token, username, role) {
  setToken(token);
  session.authenticated = true;
  session.username = username;
  session.role = role;
}

export function logout() {
  setToken('');
  session.authenticated = false;
  session.username = '';
  session.role = '';
}

export function isAdmin() {
  return session.role === 'admin';
}
