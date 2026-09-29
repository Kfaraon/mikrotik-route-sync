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

  // ---------- Live-индикатор (WebSocket) ----------
  var dot = document.getElementById("live-dot");

  function setLive(on) {
    if (!dot) return;
    dot.className = "live-dot " + (on ? "live-on" : "live-off");
    dot.title = on ? "Live: подключение активно" : "Live: нет подключения";
  }

  function connectLive() {
    if (!dot) return;
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    var ws;
    try {
      ws = new WebSocket(proto + "//" + location.host + "/api/v1/ws");
    } catch (e) {
      setLive(false);
      setTimeout(connectLive, 5000);
      return;
    }
    ws.onopen = function () { setLive(true); };
    ws.onclose = function () { setLive(false); setTimeout(connectLive, 5000); };
    ws.onerror = function () { try { ws.close(); } catch (e) {} };
  }

  if (dot) {
    setLive(false);
    connectLive();
  }

  // ---------- Бейдж проблем в заголовке вкладки ----------
  var baseTitle = document.title;

  function refreshTitleBadge() {
    fetch("/api/v1/status", {
      credentials: "same-origin",
      headers: { Accept: "application/json" }
    })
      .then(function (r) { return r.json(); })
      .then(function (j) {
        if (!j || j.ok !== true) return;
        var d = j.data || {};
        var problems = (d.degraded && d.degraded.length) || 0;
        var prefix = "";
        if (d.mikrotik_ok === false) {
          prefix = "(!) ";
        } else if (problems > 0) {
          prefix = "(" + problems + ") ";
        }
        document.title = prefix + baseTitle;
      })
      .catch(function () {});
  }

  refreshTitleBadge();
  setInterval(refreshTitleBadge, 60000);
})();