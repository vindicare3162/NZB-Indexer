// Tiny hash-based router as reactive state.
export const route = $state({ path: currentPath() });

function currentPath() {
  const h = window.location.hash.replace(/^#/, '');
  return h || '/';
}

window.addEventListener('hashchange', () => {
  route.path = currentPath();
});

export function navigate(path) {
  window.location.hash = path;
}
