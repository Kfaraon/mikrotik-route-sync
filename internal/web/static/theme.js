(function () {
  "use strict";

  function applyTheme(theme) {
    var isDark = theme === "dark";

    if (isDark) {
      document.documentElement.classList.add("dark");
    } else {
      document.documentElement.classList.remove("dark");
    }

    var buttons = document.querySelectorAll(".theme-toggle");
    for (var i = 0; i < buttons.length; i++) {
      var btn = buttons[i];
      btn.textContent = isDark ? "☀️" : "🌙";
      btn.title = isDark ? "Переключить на светлую тему" : "Переключить на тёмную тему";
      btn.setAttribute("aria-label", btn.title);
    }
  }

  function getPreferredTheme() {
    var saved = null;
    try {
      saved = localStorage.getItem("mrs-theme");
    } catch (e) {
      saved = null;
    }

    if (saved === "dark" || saved === "light") {
      return saved;
    }

    if (window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches) {
      return "dark";
    }

    return "light";
  }

  // Применяем тему сразу при загрузке скрипта
  applyTheme(getPreferredTheme());

  function init() {
    var buttons = document.querySelectorAll(".theme-toggle");

    for (var i = 0; i < buttons.length; i++) {
      (function (btn) {
        btn.addEventListener("click", function () {
          var current = document.documentElement.classList.contains("dark") ? "dark" : "light";
          var next = current === "dark" ? "light" : "dark";

          try {
            localStorage.setItem("mrs-theme", next);
          } catch (e) {
            // localStorage недоступен
          }

          applyTheme(next);
        });
      })(buttons[i]);
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
