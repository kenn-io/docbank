if (window.location.hash) {
  window.history.replaceState(null, "", `${window.location.pathname}${window.location.search}`);
}
