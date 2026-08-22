// Pre-paint theme bootstrap. Loaded as a plain synchronous <script> in <head>
// so it runs before first paint (no light-theme flash). Kept as a static file
// (not inline) so the server's CSP can stay `script-src 'self'` with no hash.
// Must mirror the key/values in src/theme/useTheme.ts.
(function () {
  try {
    var t = localStorage.getItem("orbeat-theme");
    if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
  } catch (e) {}
})();
