// Reactive session state using Svelte 5 runes.
import { getToken, setToken, api } from './api.js';

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

// hydrate restores username/role from the server when a token exists but the
// in-memory session is missing them (e.g. after a page reload, where the store
// only knows the token from storage). Without this, isAdmin() is false after a
// refresh and the Admin nav disappears until the user logs out and back in.
export async function hydrate() {
  if (!getToken() || session.role) return;
  try {
    const me = await api.me();
    session.authenticated = true;
    session.username = me.username || '';
    session.role = me.role || '';
  } catch (e) {
    // Token invalid/expired -> clear the session so the user is sent to login.
    logout();
  }
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
