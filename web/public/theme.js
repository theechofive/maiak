// Applies the saved theme before the first paint, so a dark choice never flashes light. A file,
// not an inline script: the CSP allows only same-origin scripts. Keep in sync with src/lib/theme.ts.
try {
  var theme = localStorage.getItem("maiak-theme");
  if (theme === "light" || theme === "dark") {
    document.documentElement.classList.add("theme-" + theme);
  }
} catch {
  // Storage blocked: the theme follows the system.
}
