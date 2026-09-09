// Apply the saved theme before first paint to avoid a flash. Loaded as a
// blocking script in <head>; kept separate from app.js so it runs pre-Alpine.
(function () {
  try {
    var t = localStorage.getItem('skopos:theme') || 'system';
    var dark = t === 'dark' || (t === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches);
    var el = document.documentElement;
    el.classList.toggle('dark', dark);
    el.classList.toggle('light', !dark);
  } catch (e) { }
})();
