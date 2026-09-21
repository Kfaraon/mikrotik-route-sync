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

  // ==========================================================================
  // Логи
  // ==========================================================================

  var logOutput = q("#log-output");

  if (logOutput) {
    var stats = {
      total: q("#stat-total"),
      info: q("#stat-info"),
      warn: q("#stat-warn"),
      err: q("#stat-err"),
      debug: q("#stat-debug")
    };

    var autoScroll = q("#log-autoscroll");
    var compactMode = q("#log-compact");
    var refreshBtn = q("#log-refresh");
    var clearBtn = q("#log-clear");

    var levelCounts = { INFO: 0, WARN: 0, ERROR: 0, DEBUG: 0 };
    var totalCount = 0;

    function pad(value, length) {
      var s = String(value);
      while (s.length < length) {
        s = "0" + s;
      }
      return s;
    }

    function formatLogTime(ts) {
      if (!ts) return "";
      var d = new Date(ts);
      if (isNaN(d.getTime())) return String(ts);
      return pad(d.getHours(), 2) + ":" +
        pad(d.getMinutes(), 2) + ":" +
        pad(d.getSeconds(), 2) + "." +
        pad(d.getMilliseconds(), 3);
    }

    function safeString(v) {
      if (v === null || v === undefined) return "";
      if (v instanceof Error) return v.message || String(v);
      if (typeof v === "object") {
        try {
          return JSON.stringify(v);
        } catch (e) {
          return String(v);
        }
      }
      return String(v);
    }

    function escapeHtml(value) {
      return String(value).replace(/[&<>"']/g, function (ch) {
        return {
          "&": "&amp;",
          "<": "&lt;",
          ">": "&gt;",
          "\"": "&quot;",
          "'": "&#39;"
        }[ch];
      });
    }

    function renderLogEntry(entry) {
      var level = String(entry.level || "INFO").toUpperCase();
      var levelClass = "level-" + level.toLowerCase();
      var entryClass = "log-entry";

      if (level === "ERROR") entryClass += " log-error";
      else if (level === "WARN") entryClass += " log-warn";

      totalCount++;
      if (Object.prototype.hasOwnProperty.call(levelCounts, level)) {
        levelCounts[level]++;
      }

      var rawJson;
      try {
        rawJson = JSON.stringify(entry);
      } catch (e) {
        rawJson = "{}";
      }

      var html = '<div class="' + escapeHtml(entryClass) +
        '" data-raw="' + escapeHtml(rawJson) + '">';

      if (entry.time) {
        html += '<span class="log-time">' +
          escapeHtml(formatLogTime(entry.time)) +
          "</span>";
      }

      html += '<span class="log-level ' + escapeHtml(levelClass) + '">' +
        escapeHtml(level) +
        "</span>";

      if (entry.service) {
        html += '<span class="log-service">' +
          escapeHtml(safeString(entry.service)) +
          "</span>";
      }

      if (entry.msg) {
        html += '<span class="log-msg">' +
          escapeHtml(safeString(entry.msg)) +
          "</span>";
      }

      var extras = [];
      var knownKeys = { time: true, level: true, msg: true, service: true };
      var hasError = false;
      var errorText = "";

      for (var key in entry) {
        if (!Object.prototype.hasOwnProperty.call(entry, key)) continue;
        if (Object.prototype.hasOwnProperty.call(knownKeys, key)) continue;

        var val = entry[key];

        if (key === "err" || key === "error") {
          hasError = true;
          errorText = safeString(val);
          continue;
        }

        if (key === "source") {
          if (val && typeof val === "object" && val.file) {
            extras.push({
              key: "source",
              val: String(val.file) + (val.line ? ":" + String(val.line) : "")
            });
          } else if (val) {
            extras.push({
              key: "source",
              val: safeString(val)
            });
          }
          continue;
        }

        extras.push({
          key: key,
          val: safeString(val)
        });
      }

      if (extras.length > 0 || hasError) {
        html += '<div class="log-extras">';

        if (hasError) {
          html += '<span class="log-extra-item">' +
            '<span class="log-extra-key">err=</span>' +
            '<span class="log-extra-val error-val">' +
            escapeHtml(errorText) +
            "</span>" +
            "</span>";
        }

        for (var i = 0; i < extras.length; i++) {
          html += '<span class="log-extra-item">' +
            '<span class="log-extra-key">' +
            escapeHtml(extras[i].key) +
            "=</span>" +
            '<span class="log-extra-val">' +
            escapeHtml(extras[i].val) +
            "</span>" +
            "</span>";
        }

        html += "</div>";
      }

      html += '<button type="button" class="log-copy" title="Копировать JSON" aria-label="Копировать JSON">📋</button>';
      html += "</div>";

      return html;
    }

    function updateStats() {
      if (stats.total) stats.total.textContent = String(totalCount);
      if (stats.info) stats.info.textContent = String(levelCounts.INFO);
      if (stats.warn) stats.warn.textContent = String(levelCounts.WARN);
      if (stats.err) stats.err.textContent = String(levelCounts.ERROR);
      if (stats.debug) stats.debug.textContent = String(levelCounts.DEBUG);
    }

    function resetStats() {
      levelCounts = { INFO: 0, WARN: 0, ERROR: 0, DEBUG: 0 };
      totalCount = 0;
      updateStats();
    }

    function setRefreshing(isRefreshing) {
      if (!refreshBtn) return;

      if (isRefreshing) {
        refreshBtn.classList.add("loading");
        refreshBtn.textContent = "Загрузка…";
      } else {
        refreshBtn.classList.remove("loading");
        refreshBtn.innerHTML = "⟳ Обновить";
      }
    }

    function fallbackCopy(text, btn) {
      var textarea = document.createElement("textarea");
      textarea.value = text;
      textarea.setAttribute("readonly", "");
      textarea.style.position = "fixed";
      textarea.style.top = "-1000px";
      textarea.style.left = "-1000px";
      textarea.style.opacity = "0";
      document.body.appendChild(textarea);
      textarea.select();

      try {
        document.execCommand("copy");
        if (btn) {
          var orig = btn.textContent;
          btn.textContent = "✓";
          setTimeout(function () {
            btn.textContent = orig;
          }, 1500);
        }
      } catch (e) {
        showError("Не удалось скопировать");
      }

      document.body.removeChild(textarea);
    }

    function copyToClipboard(text, btn) {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () {
          if (btn) {
            var orig = btn.textContent;
            btn.textContent = "✓";
            setTimeout(function () {
              btn.textContent = orig;
            }, 1500);
          }
        }).catch(function () {
          fallbackCopy(text, btn);
        });
      } else {
        fallbackCopy(text, btn);
      }
    }

    function buildLogsUrl(limitOverride) {
      var limit = limitOverride || ((q("#log-limit") || {}).value || "100");
      var level = (q("#log-level") || {}).value || "";
      var service = (q("#log-service") || {}).value || "";

      return "/api/v1/logs?limit=" + encodeURIComponent(limit) +
        "&level=" + encodeURIComponent(level) +
        "&service=" + encodeURIComponent(service);
    }

    function loadLogs() {
      setRefreshing(true);

      fetch(buildLogsUrl(), { credentials: "same-origin" })
        .then(function (r) {
          return r.json();
        })
        .then(function (j) {
          if (!j.ok) {
            logOutput.innerHTML = '<div class="log-error-msg">⚠ Ошибка: ' +
              escapeHtml(j.error || "неизвестная ошибка") +
              "</div>";
            resetStats();
            setRefreshing(false);
            return;
          }

          var entries = Array.isArray(j.data) ? j.data : [];

          if (entries.length === 0) {
            logOutput.innerHTML = '<div class="log-placeholder">Лог пуст или нет записей, соответствующих фильтру.</div>';
            resetStats();
            setRefreshing(false);
            return;
          }

          resetStats();

          var html = "";

          // API возвращает свежие записи первыми.
          // В UI показываем хронологически: старые сверху, новые снизу.
          for (var i = entries.length - 1; i >= 0; i--) {
            html += renderLogEntry(entries[i]);
          }

          logOutput.innerHTML = html;
          updateStats();

          if (autoScroll && autoScroll.checked) {
            logOutput.scrollTop = logOutput.scrollHeight;
          }

          setRefreshing(false);
        })
        .catch(function (e) {
          logOutput.innerHTML = '<div class="log-error-msg">⚠ Ошибка загрузки: ' +
            escapeHtml(safeString(e)) +
            "</div>";
          resetStats();
          setRefreshing(false);
        });
    }

    if (refreshBtn) {
      refreshBtn.addEventListener("click", loadLogs);
    }

    var dlBtn = q("#log-download");
    if (dlBtn) {
      dlBtn.addEventListener("click", function () {
        var a = document.createElement("a");
        a.href = buildLogsUrl("10000");
        a.download = "mrs-logs-" +
          new Date().toISOString().slice(0, 19).replace(/:/g, "-") +
          ".json";

        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
      });
    }

    if (clearBtn) {
      clearBtn.addEventListener("htmx:afterRequest", function (evt) {
        if (evt && evt.detail && evt.detail.successful) {
          loadLogs();
        }
      });
    }

    var levelFilter = q("#log-level");
    if (levelFilter) {
      levelFilter.addEventListener("change", loadLogs);
    }

    var serviceFilter = q("#log-service");
    if (serviceFilter) {
      var serviceTimer = null;
      serviceFilter.addEventListener("input", function () {
        if (serviceTimer) clearTimeout(serviceTimer);
        serviceTimer = setTimeout(loadLogs, 400);
      });
    }

    var limitFilter = q("#log-limit");
    if (limitFilter) {
      limitFilter.addEventListener("change", loadLogs);
    }

    if (compactMode) {
      compactMode.addEventListener("change", function () {
        if (compactMode.checked) {
          logOutput.classList.add("compact");
        } else {
          logOutput.classList.remove("compact");
        }
      });

      if (compactMode.checked) {
        logOutput.classList.add("compact");
      }
    }

    logOutput.addEventListener("click", function (e) {
      var target = e.target;
      var copyBtn = target && target.closest ? target.closest(".log-copy") : null;
      if (!copyBtn) return;

      var entryEl = copyBtn.closest(".log-entry");
      if (!entryEl) return;

      var raw = entryEl.getAttribute("data-raw");
      if (raw) {
        copyToClipboard(raw, copyBtn);
      }
    });

    document.addEventListener("keydown", function (e) {
      if (!logOutput) return;
      if (!document.querySelector('a[href="/logs"].active')) return;

      if ((e.ctrlKey || e.metaKey) && e.key === "r") {
        e.preventDefault();
        loadLogs();
      }
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
      timer = setTimeout(function () {
        input.type = "password";
      }, 10000);
    });
  });

  // Подсказки "?": клик по кнопке показывает/скрывает описание настройки.
  var activePop = null;

  function closePop() {
    if (activePop) {
      activePop.remove();
      activePop = null;
    }
  }

  document.addEventListener("click", function (e) {
    var btn = e.target.closest ? e.target.closest("button.hint") : null;
    if (!btn) {
      closePop();
      return;
    }

    if (activePop && activePop._owner === btn) {
      closePop();
      return;
    }

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
