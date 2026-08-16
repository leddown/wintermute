// Applies the saved theme before first paint, to avoid a flash of the wrong
// one. It has to run ahead of the stylesheet's first use, which is why it is
// loaded in <head> rather than with the rest of the scripts at the end of the
// body.
//
// Kept as a separate file rather than an inline <script> so nothing here
// depends on a policy that allows inline script; the app this came from served
// a strict `script-src 'self'`, and a file costs nothing either way.
(function () {
  var saved = localStorage.getItem("wintermute-theme");
  if (saved) document.documentElement.dataset.theme = saved;
})();
