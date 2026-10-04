(function () {
  "use strict";

  // ---------- Тема (светлая / тёмная) ----------
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

  applyTheme(getPreferredTheme());

  function initThemeButtons() {
    var buttons = document.querySelectorAll(".theme-toggle");
    for (var i = 0; i < buttons.length; i++) {
      (function (btn) {
        btn.addEventListener("click", function () {
          var current = document.documentElement.classList.contains("dark") ? "dark" : "light";
          var next = current === "dark" ? "light" : "dark";
          try {
            localStorage.setItem("mrs-theme", next);
          } catch (e) {}
          applyTheme(next);
        });
      })(buttons[i]);
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", initThemeButtons);
  } else {
    initThemeButtons();
  }

  // ---------- Live-индикатор MikroTik + бейдж проблем ----------
  // theme.js грузится в <head> до разметки, поэтому элемент ищем только
  // после DOMContentLoaded (раньше dot оставался null и индикатор молчал).
  var dot = null;
  var baseTitle = document.title;

  function setLive(on) {
    if (!dot) return;
    dot.className = "live-dot " + (on ? "live-on" : "live-off");
    dot.title = on ? "MikroTik: подключение есть" : "MikroTik: нет подключения";
  }

  function saveMt(ok, problems) {
    try {
      localStorage.setItem("mrs_mt", JSON.stringify({ ok: ok, problems: problems | 0, ts: Date.now() }));
    } catch (e) {}
  }

  function loadMt() {
    try {
      var v = JSON.parse(localStorage.getItem("mrs_mt") || "null");
      return v && typeof v.ok === "boolean" ? v : null;
    } catch (e) {
      return null;
    }
  }

  function applyTitle(ok, problems) {
    var prefix = "";
    if (ok === false) {
      prefix = "(!) ";
    } else if (problems > 0) {
      prefix = "(" + problems + ") ";
    }
    document.title = prefix + baseTitle;
  }

  function refreshStatus() {
    fetch("/api/v1/status", {
      credentials: "same-origin",
      headers: { Accept: "application/json" }
    })
      .then(function (r) { return r.json(); })
      .then(function (j) {
        if (!j || j.ok !== true) return;
        var d = j.data || {};

        // Точка горит зелёным, пока MikroTik отвечает, и красным — когда нет.
        if (d.mikrotik_ok === true) {
          setLive(true);
        } else if (d.mikrotik_ok === false) {
          setLive(false);
        }

        var problems = (d.degraded && d.degraded.length) || 0;
        if (d.mikrotik_ok === true || d.mikrotik_ok === false) {
          saveMt(d.mikrotik_ok, problems);
        }
        applyTitle(d.mikrotik_ok, problems);
      })
      .catch(function () {
        setLive(false);
        saveMt(false, 0);
      });
  }

  function startLive() {
    dot = document.getElementById("live-dot");
    // Мгновенно восстанавливаем последнее известное состояние, пока не
    // пришёл ответ /api/v1/status — иначе при переходе между страницами
    // индикатор успевает показать серый.
    var saved = loadMt();
    if (saved) {
      setLive(saved.ok);
      applyTitle(saved.ok, saved.problems);
    }
    refreshStatus();
    setInterval(refreshStatus, 15000);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", startLive);
  } else {
    startLive();
  }
})();