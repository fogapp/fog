// Resolve the stored appearance before first paint so nobody sees a flash
// of the wrong theme on the way in. Defaults — light, wearing Graphite; dark
// wearing Fog — are repeated from src/theme.ts.
//
// A file of its own, loaded by a plain blocking <script src>, not an inline
// script: fog's Content-Security-Policy allows scripts from 'self' only
// (internal/spa/headers.go). Both index.html files load it — the dashboard
// from public/, the setup wizard via SETUP_PUBLIC in vite.config.setup.ts.
(function () {
  try {
    var p = localStorage.getItem("fog.theme");
    var t =
      p === "light" || p === "dark"
        ? p
        : p === "system"
          ? matchMedia("(prefers-color-scheme: light)").matches
            ? "light"
            : "dark"
          : "light";
    document.documentElement.dataset.theme = t;
    document.documentElement.dataset.palette =
      localStorage.getItem("fog.palette." + t) || (t === "light" ? "graphite" : "fog");
    if (t === "dark") {
      var m = document.querySelector('meta[name="theme-color"]');
      if (m) m.setAttribute("content", "#0d1116");
    }
  } catch (e) {}
})();
