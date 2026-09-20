(function () {
  "use strict";

  function csrf() {
    var m = document.querySelector('meta[name="csrf-token"]');
    return m ? m.content : "";
  }

  if (window.htmx) {
    document.body.addEventListener("htmx:configRequest", function (evt) {
      evt.detail.headers["X-CSRF-Token"] = csrf();
    });
    document.body.addEventListener("htmx:responseError", function (evt) {
      var xhr = evt.detail.xhr || {};
      showError("Ошибка " + (xhr.status || "") + ": " +
        (xhr.responseText || xhr.statusText || "запрос не выполнен"));
    });
    document.body.addEventListener("htmx:sendError", function (evt) {
      showError("Ошибка сети: " + ((evt.detail && evt.detail.error) || "не удалось отправить запрос"));
    });
  }

  function showError(msg) {
    var t = document.getElementById("mrs-toast");
    if (!t) {
      t = document.createElement("div");
      t.id = "mrs-toast";
      t.className = "toast";
      document.body.appendChild(t);
    }
    t.textContent = msg;
    t.classList.add("show");
    clearTimeout(t._timer);
    t._timer = setTimeout(function () { t.classList.remove("show"); }, 9000);
  }

  var q = function (s) { return document.querySelector(s); };

  var all = q("#check-all");
  if (all) {
    all.addEventListener("change", function () {
      document.querySelectorAll('input[name="service"]').forEach(function (c) {
        c.checked = all.checked;
      });
    });
  }

  function connectWS() {
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    var ws;
    try {
      ws = new WebSocket(proto + "//" + location.host + "/api/v1/ws");
    } catch (e) {
      return;
    }
    ws.onmessage = function (ev) {
      var box = q("#events");
      if (!box) return;
      try {
        var msg = JSON.parse(ev.data);
        var line = document.createElement("div");
        var t = new Date(msg.time).toLocaleTimeString();
        line.textContent = t + " " + msg.event + " " + JSON.stringify(msg.data);
        box.prepend(line);
        while (box.childElementCount > 20) box.lastChild.remove();
      } catch (e) {}
    };
    ws.onclose = function () { setTimeout(connectWS, 5000); };
  }
  connectWS();

  var out = q("#log-output");
  if (out) {
    function loadLogs() {
      var limit = (q("#log-limit") || {}).value || "100";
      var level = (q("#log-level") || {}).value || "";
      var service = (q("#log-service") || {}).value || "";
      var url = "/api/v1/logs?limit=" + encodeURIComponent(limit) +
        "&level=" + encodeURIComponent(level) +
        "&service=" + encodeURIComponent(service);
      fetch(url, { credentials: "same-origin" })
        .then(function (r) { return r.json(); })
        .then(function (j) {
          if (!j.ok) { out.textContent = "Ошибка: " + j.error; return; }
          out.textContent = (j.data || []).map(function (e) {
            return JSON.stringify(e);
          }).join("\n");
        })
        .catch(function (e) { out.textContent = "Ошибка загрузки: " + e; });
    }
    var rb = q("#log-refresh");
    if (rb) rb.addEventListener("click", loadLogs);
    var dl = q("#log-download");
    if (dl) dl.addEventListener("click", function () {
      var a = document.createElement("a");
      a.href = "/api/v1/logs?limit=10000";
      a.download = "mrs-logs.json";
      a.click();
    });
    loadLogs();
  }

  document.querySelectorAll(".secret-field").forEach(function (el) {
    var input = el.querySelector("input");
    var btn = el.querySelector("button");
    if (!input || !btn) return;
    var timer = null;
    btn.addEventListener("click", function () {
      input.type = "text";
      clearTimeout(timer);
      timer = setTimeout(function () { input.type = "password"; }, 10000);
    });
  });

  // Подсказки "?": клик по кнопке показывает/скрывает описание настройки.
  var activePop = null;
  function closePop() {
    if (activePop) { activePop.remove(); activePop = null; }
  }
  document.addEventListener("click", function (e) {
    var btn = e.target.closest ? e.target.closest("button.hint") : null;
    if (!btn) { closePop(); return; }
    if (activePop && activePop._owner === btn) { closePop(); return; }
    closePop();
    var pop = document.createElement("div");
    pop.className = "pop";
    pop.textContent = btn.getAttribute("data-help") || "";
    var host = btn.closest(".setting-label") || btn.parentElement;
    host.style.position = "relative";
    host.appendChild(pop);
    pop._owner = btn;
    activePop = pop;
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") closePop();
  });
})();
