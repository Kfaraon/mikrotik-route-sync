(function () {
  "use strict";

  var page = document.getElementById("dashboard-page");
  if (!page) return;

  function byId(id) {
    return document.getElementById(id);
  }

  function esc(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (ch) {
      return {
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        "\"": "&quot;",
        "'": "&#39;"
      }[ch];
    });
  }

  function text(value) {
    if (value === null || value === undefined) return "";
    if (typeof value === "object") {
      try {
        return JSON.stringify(value);
      } catch (e) {
        return String(value);
      }
    }
    return String(value);
  }

  function trim(value) {
    return text(value).trim();
  }

  function lower(value) {
    return trim(value).toLowerCase();
  }

  function pad(n) {
    return n < 10 ? "0" + n : String(n);
  }

  function plural(n, forms) {
    n = Math.abs(Number(n) || 0) % 100;
    var n1 = n % 10;

    if (n > 10 && n < 20) return forms[2];
    if (n1 > 1 && n1 < 5) return forms[1];
    if (n1 === 1) return forms[0];
    return forms[2];
  }

  function objKeys(obj) {
    return Object.keys(obj || {});
  }

  function toArray(value) {
    if (Array.isArray(value)) return value;
    if (value && Array.isArray(value.records)) return value.records;
    if (value && Array.isArray(value.items)) return value.items;
    if (value && Array.isArray(value.services)) return value.services;
    return [];
  }

  function api(path, options) {
    options = options || {};
    options.credentials = "same-origin";
    options.headers = options.headers || {};
    options.headers.Accept = "application/json";

    return fetch(path, options).then(function (response) {
      return response.text().then(function (body) {
        var parsed = null;

        try {
          parsed = body ? JSON.parse(body) : null;
        } catch (e) {
          parsed = null;
        }

        if (!response.ok || !parsed || parsed.ok !== true) {
          throw new Error(parsed && parsed.error ? parsed.error : "HTTP " + response.status);
        }

        return parsed.data;
      });
    });
  }

  var els = {
    alert: byId("dash-alert"),
    refresh: byId("dash-refresh"),
    auto: byId("dash-auto-refresh"),
    health: byId("health-list"),
    next: byId("next-list")
  };

  var state = {
    status: null,
    services: [],
    schedules: {
      global: "",
      groups: {},
      services: {},
      effective: {}
    },
    history: [],
    logs: [],
    derived: null,
    loading: false,
    autoTimer: null
  };

  function setAlert(kind, message) {
    if (!els.alert) return;

    if (!message) {
      els.alert.hidden = true;
      els.alert.className = "dash-alert";
      els.alert.textContent = "";
      return;
    }

    els.alert.hidden = false;
    els.alert.className = "dash-alert " + (kind || "warn");
    els.alert.textContent = message;
  }

  function setRefreshing(loading) {
    state.loading = loading;

    if (!els.refresh) return;

    els.refresh.disabled = loading;
    els.refresh.textContent = loading ? "Загрузка…" : "Обновить";
  }

  function parseTime(value) {
    if (!value) return null;

    if (typeof value === "number") {
      var d = new Date(value > 1000000000000 ? value : value * 1000);
      return isNaN(d.getTime()) ? null : d;
    }

    var parsed = new Date(value);
    return isNaN(parsed.getTime()) ? null : parsed;
  }

  function sameDay(a, b) {
    return a.getFullYear() === b.getFullYear() &&
      a.getMonth() === b.getMonth() &&
      a.getDate() === b.getDate();
  }

  function formatNextTime(d) {
    if (!d) return "—";

    var now = new Date();

    if (sameDay(d, now)) {
      return pad(d.getHours()) + ":" + pad(d.getMinutes());
    }

    return pad(d.getDate()) + "." + pad(d.getMonth() + 1) + " " +
      pad(d.getHours()) + ":" + pad(d.getMinutes());
  }

  function relativeTime(value) {
    var d = parseTime(value);
    if (!d) return "—";

    var diff = Date.now() - d.getTime();
    if (diff < 0) return "в будущем";

    var sec = Math.floor(diff / 1000);
    if (sec < 45) return "только что";

    var min = Math.floor(sec / 60);
    if (min < 60) {
      return min + " " + plural(min, ["минута", "минуты", "минут"]) + " назад";
    }

    var hr = Math.floor(min / 60);
    if (hr < 24) {
      return hr + " " + plural(hr, ["час", "часа", "часов"]) + " назад";
    }

    var day = Math.floor(hr / 24);
    return day + " " + plural(day, ["день", "дня", "дней"]) + " назад";
  }

  function relativeNext(d) {
    if (!d) return "";

    var diff = d.getTime() - Date.now();
    if (diff < 0) diff = 0;

    var sec = Math.floor(diff / 1000);
    if (sec < 60) return "меньше минуты";

    var min = Math.floor(sec / 60);
    if (min < 60) {
      return "через " + min + " " + plural(min, ["минута", "минуты", "минут"]);
    }

    var hr = Math.floor(min / 60);
    if (hr < 24) {
      return "через " + hr + " " + plural(hr, ["час", "часа", "часов"]);
    }

    var day = Math.floor(hr / 24);
    return "через " + day + " " + plural(day, ["день", "дня", "дней"]);
  }

  function normalizeSpec(value) {
    return trim(value);
  }

  function parseDuration(spec) {
    var m = normalizeSpec(spec).match(/^every\s+(\d+)\s*(m|h|d|w)$/i);
    if (!m) return 0;

    var n = parseInt(m[1], 10);
    if (!n || n < 0) return 0;

    var unit = m[2].toLowerCase();

    if (unit === "m") return n * 60 * 1000;
    if (unit === "h") return n * 60 * 60 * 1000;
    if (unit === "d") return n * 24 * 60 * 60 * 1000;
    if (unit === "w") return n * 7 * 24 * 60 * 60 * 1000;

    return 0;
  }

  function formatIntervalHuman(ms) {
    var min = 60 * 1000;
    var hour = 60 * min;
    var day = 24 * hour;
    var week = 7 * day;

    if (ms % week === 0) {
      var w = ms / week;
      return w === 1 ? "каждую неделю" : "каждые " + w + " " + plural(w, ["неделя", "недели", "недель"]);
    }

    if (ms % day === 0) {
      var d = ms / day;
      return d === 1 ? "каждый день" : "каждые " + d + " " + plural(d, ["день", "дня", "дней"]);
    }

    if (ms % hour === 0) {
      var h = ms / hour;
      return h === 1 ? "каждый час" : "каждые " + h + " " + plural(h, ["час", "часа", "часов"]);
    }

    if (ms % min === 0) {
      var m = ms / min;
      return m === 1 ? "каждую минуту" : "каждые " + m + " " + plural(m, ["минута", "минуты", "минут"]);
    }

    return "каждые " + Math.round(ms / 1000) + " сек.";
  }

  function isCronLike(spec) {
    var s = normalizeSpec(spec);
    var parts = s.split(/\s+/);
    return parts.length === 5 && /^[\d*\/,\-\s]+$/.test(s);
  }

  function humanSchedule(spec) {
    var s = normalizeSpec(spec);

    if (!s) return "не задано";

    var l = s.toLowerCase();

    if (l === "manual") return "только вручную";
    if (l === "disabled") return "отключено";
    if (l === "inherit") return "наследуется";

    var ms = parseDuration(s);
    if (ms > 0) return formatIntervalHuman(ms);

    var daily = s.match(/^daily at (\d{1,2}):(\d{2})$/i);
    if (daily) {
      return "ежедневно в " + daily[1] + ":" + daily[2];
    }

    var weekly = s.match(/^weekly on ([a-z]+) at (\d{1,2}):(\d{2})$/i);
    if (weekly) {
      var days = {
        sunday: "воскресеньям",
        monday: "понедельникам",
        tuesday: "вторникам",
        wednesday: "средам",
        thursday: "четвергам",
        friday: "пятницам",
        saturday: "субботам"
      };

      var day = weekly[1].toLowerCase();
      return "по " + (days[day] || day) + " в " + weekly[2] + ":" + weekly[3];
    }

    if (isCronLike(s)) {
      return "по cron: " + s;
    }

    return s;
  }

  function nextRun(spec) {
    var s = normalizeSpec(spec);
    if (!s) return null;

    var l = s.toLowerCase();
    if (l === "manual" || l === "disabled" || l === "inherit") return null;

    var ms = parseDuration(s);
    if (ms > 0) return new Date(Date.now() + ms);

    var daily = s.match(/^daily at (\d{1,2}):(\d{2})$/i);
    if (daily) {
      var dh = parseInt(daily[1], 10);
      var dm = parseInt(daily[2], 10);

      if (dh < 0 || dh > 23 || dm < 0 || dm > 59) return null;

      var now = new Date();
      var d = new Date(now.getFullYear(), now.getMonth(), now.getDate(), dh, dm, 0, 0);

      if (d.getTime() <= now.getTime()) {
        d.setDate(d.getDate() + 1);
      }

      return d;
    }

    var weekly = s.match(/^weekly on ([a-z]+) at (\d{1,2}):(\d{2})$/i);
    if (weekly) {
      var weekdays = {
        sunday: 0,
        monday: 1,
        tuesday: 2,
        wednesday: 3,
        thursday: 4,
        friday: 5,
        saturday: 6
      };

      var target = weekdays[weekly[1].toLowerCase()];
      if (target === undefined) return null;

      var wh = parseInt(weekly[2], 10);
      var wm = parseInt(weekly[3], 10);

      if (wh < 0 || wh > 23 || wm < 0 || wm > 59) return null;

      var n = new Date();
      var wd = new Date(n.getFullYear(), n.getMonth(), n.getDate(), wh, wm, 0, 0);
      var delta = (target - wd.getDay() + 7) % 7;

      wd.setDate(wd.getDate() + delta);

      if (wd.getTime() <= n.getTime()) {
        wd.setDate(wd.getDate() + 7);
      }

      return wd;
    }

    return null;
  }

  function serviceName(row) {
    return text(row && (row.Name || row.name || row.Service || row.service));
  }

  function serviceRoutes(row) {
    if (!row) return 0;

    var v = row.Routes;
    if (v === undefined) v = row.routes;

    return Number(v) || 0;
  }

  function serviceSchedule(row) {
    if (!row) return "";
    return text(row.Schedule || row.schedule);
  }

  function getEffective(name) {
    var n = text(name);
    var ln = lower(n);
    var eff = state.schedules && state.schedules.effective;

    if (eff && typeof eff === "object") {
      if (eff[n]) return normalizeSpec(eff[n]);
      if (eff[ln]) return normalizeSpec(eff[ln]);

      for (var k in eff) {
        if (Object.prototype.hasOwnProperty.call(eff, k) && lower(k) === ln) {
          return normalizeSpec(eff[k]);
        }
      }
    }

    for (var i = 0; i < state.services.length; i++) {
      if (lower(serviceName(state.services[i])) === ln) {
        return normalizeSpec(serviceSchedule(state.services[i]));
      }
    }

    return normalizeSpec(state.schedules && state.schedules.global);
  }

  function historyTime(rec) {
    if (!rec) return "";

    return rec.time ||
      rec.started_at ||
      rec.finished_at ||
      rec.timestamp ||
      rec.created_at ||
      rec.Time ||
      rec.StartedAt ||
      rec.FinishedAt ||
      "";
  }

  function historyService(rec) {
    if (!rec) return "";
    return text(rec.service || rec.name || rec.Service || rec.Name);
  }

  function historyError(rec) {
    if (!rec) return "";
    return text(rec.error || rec.err || rec.message || rec.reason || rec.Error || rec.Err);
  }

  function historyStatus(rec) {
    if (!rec) return "unknown";

    if (historyError(rec)) return "error";

    var st = lower(rec.status || rec.result || rec.state || rec.Status);

    if (st === "error" || st === "failed" || st === "fail") return "error";
    if (st === "warning" || st === "warn") return "warning";
    if (st === "ok" || st === "success" || st === "completed") return "success";
    if (st === "running" || st === "started") return "running";

    if (rec.ok === false) return "error";
    if (rec.ok === true) return "success";

    return "unknown";
  }

  function statusLabel(status) {
    if (status === "success") return "успех";
    if (status === "error") return "ошибка";
    if (status === "warning") return "предупреждение";
    if (status === "running") return "выполняется";
    return "нет данных";
  }

  function statusClass(status) {
    if (status === "success") return "ok";
    if (status === "error") return "err";
    if (status === "warning") return "warn";
    return "muted";
  }

  function logLevel(entry) {
    return text(entry && (entry.level || entry.Level)).toUpperCase() || "INFO";
  }

  function logService(entry) {
    return text(entry && (entry.service || entry.Service));
  }

  function buildDerived() {
    var degraded = {};
    var deg = state.status && state.status.degraded;

    if (Array.isArray(deg)) {
      for (var i = 0; i < deg.length; i++) {
        var item = deg[i];
        var name = "";

        if (typeof item === "string") {
          name = item;
        } else if (item) {
          name = item.service || item.name || item.Service || item.Name;
        }

        name = lower(name);
        if (name) degraded[name] = true;
      }
    }

    var errorSvc = {};
    var warnSvc = {};

    for (var j = 0; j < state.logs.length; j++) {
      var entry = state.logs[j];
      var svc = lower(logService(entry));

      if (!svc) continue;

      var level = logLevel(entry);

      if (level === "ERROR") errorSvc[svc] = true;
      else if (level === "WARN") warnSvc[svc] = true;
    }

    var sortedHistory = state.history.slice().sort(function (a, b) {
      var ta = parseTime(historyTime(a));
      var tb = parseTime(historyTime(b));

      if (!ta && !tb) return 0;
      if (!ta) return 1;
      if (!tb) return -1;

      return tb.getTime() - ta.getTime();
    });

    var latestByService = {};

    for (var k = 0; k < sortedHistory.length; k++) {
      var rec = sortedHistory[k];
      var recService = lower(historyService(rec));

      if (recService && !latestByService[recService]) {
        latestByService[recService] = rec;
      }
    }

    for (var nameKey in latestByService) {
      if (!Object.prototype.hasOwnProperty.call(latestByService, nameKey)) continue;

      var st = historyStatus(latestByService[nameKey]);

      if (st === "error") errorSvc[nameKey] = true;
      else if (st === "warning") warnSvc[nameKey] = true;
    }

    var problem = {};

    for (var dk in degraded) {
      if (Object.prototype.hasOwnProperty.call(degraded, dk)) problem[dk] = true;
    }

    for (var ek in errorSvc) {
      if (Object.prototype.hasOwnProperty.call(errorSvc, ek)) problem[ek] = true;
    }

    for (var wk in warnSvc) {
      if (Object.prototype.hasOwnProperty.call(warnSvc, wk)) problem[wk] = true;
    }

    var routes = 0;

    for (var s = 0; s < state.services.length; s++) {
      routes += serviceRoutes(state.services[s]);
    }

    state.derived = {
      degraded: degraded,
      errorSvc: errorSvc,
      warnSvc: warnSvc,
      latestByService: latestByService,
      latestHistory: sortedHistory[0] || null,
      totalRoutes: routes,
      problemCount: objKeys(problem).length
    };
  }

  function healthRow(label, value, note, cls) {
    return '<div class="health-row ' + esc(cls || "muted") + '">' +
      "<span>" + esc(label) + "</span>" +
      "<strong>" + esc(value) + "</strong>" +
      "<small>" + esc(note) + "</small>" +
      "</div>";
  }

  function renderHealth() {
    if (!els.health) return;

    var d = state.derived || {};
    var degraded = d.degraded || {};
    var errorSvc = d.errorSvc || {};
    var warnSvc = d.warnSvc || {};

    var mikrotikValue;
    var mikrotikNote;
    var mikrotikCls;

    if (!state.status) {
      mikrotikValue = "Нет данных";
      mikrotikNote = "статус недоступен";
      mikrotikCls = "muted";
    } else if (state.status.mikrotik_ok === false) {
      mikrotikValue = "Недоступен";
      mikrotikNote = "проверьте REST API и сеть";
      mikrotikCls = "err";
    } else {
      mikrotikValue = "Доступен";
      mikrotikNote = "Uptime: " + text(state.status.uptime || "—");
      mikrotikCls = "ok";
    }

    var total = state.services.length;
    var routes = d.totalRoutes || 0;

    var servicesNote;

    if (total === 0) {
      servicesNote = "сервисы не добавлены";
    } else if (routes === 0) {
      servicesNote = "маршрутов пока нет";
    } else {
      servicesNote = routes + " " + plural(routes, ["маршрут", "маршрута", "маршрутов"]);
    }

    var degradedCount = objKeys(degraded).length;
    var errorCount = objKeys(errorSvc).length;
    var warnCount = objKeys(warnSvc).length;
    var problemCount = d.problemCount || 0;

    var problemParts = [];

    if (degradedCount) {
      problemParts.push("сбойных: " + degradedCount);
    }

    if (errorCount) {
      problemParts.push("ошибок: " + errorCount);
    }

    if (warnCount) {
      problemParts.push("предупреждений: " + warnCount);
    }

    var problemNote = problemParts.length
      ? problemParts.join(" · ")
      : "проблем не обнаружено";

    var problemCls = "ok";

    if (problemCount) {
      problemCls = (degradedCount || errorCount) ? "err" : "warn";
    }

    var latest = d.latestHistory;
    var latestValue = latest ? (historyService(latest) || "—") : "—";
    var latestStatus = latest ? historyStatus(latest) : "unknown";
    var latestNote = latest
      ? statusLabel(latestStatus) + " · " + relativeTime(historyTime(latest))
      : "история пуста";

    var html = "";

    html += healthRow("RouterOS API", mikrotikValue, mikrotikNote, mikrotikCls);
    html += healthRow("Сервисы", String(total), servicesNote, "muted");
    html += healthRow("Проблемы", String(problemCount), problemNote, problemCls);
    html += healthRow("Последняя синхронизация", latestValue, latestNote, latest ? statusClass(latestStatus) : "muted");

    els.health.innerHTML = html;
  }

  function renderNext() {
    if (!els.next) return;

    var items = [];

    for (var i = 0; i < state.services.length; i++) {
      var name = serviceName(state.services[i]);
      if (!name) continue;

      var effective = getEffective(name);
      var next = nextRun(effective);

      if (next) {
        items.push({
          name: name,
          effective: effective,
          next: next
        });
      }
    }

    items.sort(function (a, b) {
      return a.next.getTime() - b.next.getTime() || a.name.localeCompare(b.name);
    });

    if (!items.length) {
      els.next.innerHTML = '<div class="empty-state">Нет запланированных запусков.</div>';
      return;
    }

    var limit = Math.min(items.length, 8);
    var html = "";

    for (var j = 0; j < limit; j++) {
      var item = items[j];

      html += '<div class="list-row">' +
        '<span class="activity-time">' + esc(formatNextTime(item.next)) + "</span>" +
        "<div>" +
          '<div class="primary-text">' + esc(item.name) + "</div>" +
          '<div class="secondary-text">' + esc(humanSchedule(item.effective)) + "</div>" +
        "</div>" +
        '<div class="activity-right">' +
          '<span class="muted">' + esc(relativeNext(item.next)) + "</span>" +
        "</div>" +
        "</div>";
    }

    els.next.innerHTML = html;
  }

  function loadAll(force) {
    if (state.loading && !force) return Promise.resolve();

    setRefreshing(true);

    return Promise.allSettled([
      api("/api/v1/status"),
      api("/api/v1/services"),
      api("/api/v1/schedules"),
      api("/api/v1/history?limit=120"),
      api("/api/v1/logs?limit=250")
    ]).then(function (results) {
      var labels = {
        status: "статус",
        services: "сервисы",
        schedules: "расписания",
        history: "история",
        logs: "логи"
      };

      var failed = [];

      if (results[0].status === "fulfilled") {
        state.status = results[0].value || null;
      } else {
        state.status = null;
        failed.push(labels.status);
      }

      if (results[1].status === "fulfilled") {
        state.services = toArray(results[1].value);
      } else {
        state.services = [];
        failed.push(labels.services);
      }

      if (results[2].status === "fulfilled") {
        state.schedules = results[2].value || {
          global: "",
          groups: {},
          services: {},
          effective: {}
        };
      } else {
        state.schedules = {
          global: "",
          groups: {},
          services: {},
          effective: {}
        };
        failed.push(labels.schedules);
      }

      if (results[3].status === "fulfilled") {
        state.history = toArray(results[3].value);
      } else {
        state.history = [];
        failed.push(labels.history);
      }

      if (results[4].status === "fulfilled") {
        state.logs = toArray(results[4].value);
      } else {
        state.logs = [];
        failed.push(labels.logs);
      }

      if (failed.length) {
        setAlert(
          failed.length >= 4 ? "error" : "warn",
          "Не удалось загрузить: " + failed.join(", ")
        );
      } else {
        setAlert("", "");
      }

      buildDerived();
      renderHealth();
      renderNext();
    }).catch(function (err) {
      setAlert("error", "Не удалось обновить дашборд: " + text(err && err.message));
    }).then(function () {
      setRefreshing(false);
    });
  }

  function scheduleAuto() {
    if (state.autoTimer) {
      clearInterval(state.autoTimer);
      state.autoTimer = null;
    }

    if (els.auto && els.auto.checked) {
      state.autoTimer = setInterval(function () {
        if (document.visibilityState === "visible" && !state.loading) {
          loadAll(false);
        }
      }, 60000);
    }
  }

  function bindEvents() {
    if (els.refresh) {
      els.refresh.addEventListener("click", function () {
        loadAll(true);
      });
    }

    if (els.auto) {
      els.auto.addEventListener("change", scheduleAuto);
    }

    document.addEventListener("keydown", function (e) {
      if ((e.ctrlKey || e.metaKey) && e.key === "r") {
        e.preventDefault();
        loadAll(true);
      }
    });
  }

  function init() {
    bindEvents();
    loadAll(true);
    scheduleAuto();
  }

  init();
})();
