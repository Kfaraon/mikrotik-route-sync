(function () {
  "use strict";

  var page = document.getElementById("dashboard-page");
  if (!page) return;

  function byId(id) {
    return document.getElementById(id);
  }

  function qs(sel) {
    return document.querySelector(sel);
  }

  function qsa(sel) {
    return document.querySelectorAll(sel);
  }

  function each(list, fn) {
    if (!list) return;
    for (var i = 0; i < list.length; i++) {
      fn(list[i], i);
    }
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

  function truncate(value, max) {
    var s = text(value);
    if (s.length <= max) return s;
    return s.slice(0, max - 1) + "…";
  }

  function pad(n) {
    return n < 10 ? "0" + n : String(n);
  }

  function lower(value) {
    return text(value).trim().toLowerCase();
  }

  function csrf() {
    var m = qs('meta[name="csrf-token"]');
    return m ? m.content : "";
  }

  function closest(el, selector) {
    return el && el.closest ? el.closest(selector) : null;
  }

  function showToast(message, isError) {
    var t = byId("mrs-toast");
    if (!t) {
      t = document.createElement("div");
      t.id = "mrs-toast";
      t.className = "toast";
      document.body.appendChild(t);
    }

    t.textContent = message;
    t.className = "toast show" + (isError ? "" : " success");

    clearTimeout(t._timer);
    t._timer = setTimeout(function () {
      t.className = "toast";
    }, 5000);
  }

  var els = {
    alert: byId("dash-alert"),
    refresh: byId("refresh-all"),
    toggleLive: byId("toggle-live"),
    autoRefresh: byId("auto-refresh"),
    selectedCount: byId("selected-count"),
    health: byId("health-list"),
    next: byId("next-list"),
    servicesBody: byId("services-body"),
    servicesEmpty: byId("services-empty"),
    serviceSearch: byId("service-search"),
    serviceFilter: byId("service-filter"),
    selectAll: byId("select-all"),
    activityList: byId("activity-list"),
    tabs: qsa(".tabs .tab"),
    actionSyncAll: byId("action-sync-all"),
    actionDryAll: byId("action-dry-all"),
    actionSyncSelected: byId("action-sync-selected"),
    actionDrySelected: byId("action-dry-selected"),
    kpi: {
      router: byId("kpi-router"),
      routerValue: byId("kpi-router-value"),
      routerNote: byId("kpi-router-note"),
      services: byId("kpi-services"),
      servicesValue: byId("kpi-services-value"),
      servicesNote: byId("kpi-services-note"),
      routes: byId("kpi-routes"),
      routesValue: byId("kpi-routes-value"),
      routesNote: byId("kpi-routes-note"),
      next: byId("kpi-next"),
      nextValue: byId("kpi-next-value"),
      nextNote: byId("kpi-next-note"),
      last: byId("kpi-last"),
      lastValue: byId("kpi-last-value"),
      lastNote: byId("kpi-last-note"),
      issues: byId("kpi-issues"),
      issuesValue: byId("kpi-issues-value"),
      issuesNote: byId("kpi-issues-note")
    }
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
    sortedHistory: [],
    historyByService: {},
    logs: [],
    issueLogs: [],
    live: [],
    degraded: {},
    selection: {},
    visibleItems: [],
    visibleKeys: [],
    activeTab: "sync",
    loading: false,
    ws: null,
    liveUserEnabled: false,
    autoTimer: null
  };

  function api(path, options) {
    options = options || {};
    options.credentials = "same-origin";
    options.headers = options.headers || {};
    options.headers.Accept = "application/json";

    var method = options.method ? options.method.toUpperCase() : "GET";
    if (method !== "GET") {
      options.headers["X-CSRF-Token"] = csrf();
      if (options.body && !options.headers["Content-Type"]) {
        options.headers["Content-Type"] = "application/json";
      }
    }

    return fetch(path, options).then(function (response) {
      return response.text().then(function (body) {
        var parsed = null;
        try {
          parsed = body ? JSON.parse(body) : null;
        } catch (e) {
          parsed = null;
        }

        if (!response.ok || !parsed || parsed.ok !== true) {
          var msg = parsed && parsed.error ? parsed.error : "HTTP " + response.status;
          throw new Error(msg);
        }

        return parsed.data;
      });
    });
  }

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
    els.refresh.textContent = loading ? "Загрузка…" : "⟳ Обновить";

    if (loading) {
      els.refresh.classList.add("loading");
    } else {
      els.refresh.classList.remove("loading");
    }
  }

  function setKpi(key, value, note, stateName) {
    var card = els.kpi[key];
    var valueEl = els.kpi[key + "Value"];
    var noteEl = els.kpi[key + "Note"];

    if (!card || !valueEl || !noteEl) return;

    card.className = "kpi-card" + (stateName ? " state-" + stateName : "");
    valueEl.textContent = text(value);
    noteEl.textContent = text(note);
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

  function formatClock(value) {
    var d = parseTime(value);
    if (!d) return "—";
    return pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
  }

  function formatDateTime(d) {
    if (!d) return "—";
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
    if (min < 60) return min + " мин назад";

    var hr = Math.floor(min / 60);
    if (hr < 24) return hr + " ч назад";

    var day = Math.floor(hr / 24);
    return day + " дн назад";
  }

  function relativeNext(d) {
    if (!d) return "";

    var diff = d.getTime() - Date.now();
    if (diff < 0) diff = 0;

    var sec = Math.floor(diff / 1000);
    if (sec < 60) return "меньше минуты";

    var min = Math.floor(sec / 60);
    if (min < 60) return "через " + min + " мин";

    var hr = Math.floor(min / 60);
    if (hr < 24) return "через " + hr + " ч";

    var day = Math.floor(hr / 24);
    return "через " + day + " дн";
  }

  function normalizeSpec(value) {
    return text(value).trim();
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

  function formatDurationMs(ms) {
    if (!ms) return "";

    if (ms >= 7 * 24 * 60 * 60 * 1000 && ms % (7 * 24 * 60 * 60 * 1000) === 0) {
      return (ms / (7 * 24 * 60 * 60 * 1000)) + " нед.";
    }
    if (ms >= 24 * 60 * 60 * 1000 && ms % (24 * 60 * 60 * 1000) === 0) {
      return (ms / (24 * 60 * 60 * 1000)) + " дн.";
    }
    if (ms >= 60 * 60 * 1000 && ms % (60 * 60 * 1000) === 0) {
      return (ms / (60 * 60 * 1000)) + " ч.";
    }
    if (ms >= 60 * 1000 && ms % (60 * 1000) === 0) {
      return (ms / (60 * 1000)) + " мин.";
    }

    return Math.round(ms / 1000) + " сек.";
  }

  function isCronLike(spec) {
    var s = normalizeSpec(spec);
    var parts = s.split(/\s+/);
    return parts.length === 5 && /^[\d*\/,\-\s]+$/.test(s);
  }

  function classify(spec) {
    var s = normalizeSpec(spec);

    if (!s) {
      return { key: "empty", label: "не задано", className: "badge-muted" };
    }

    var lowerSpec = s.toLowerCase();

    if (lowerSpec === "manual") {
      return { key: "manual", label: "вручную", className: "badge-manual" };
    }

    if (lowerSpec === "disabled") {
      return { key: "disabled", label: "отключено", className: "badge-disabled" };
    }

    if (lowerSpec === "inherit") {
      return { key: "inherit", label: "наследуется", className: "badge-muted" };
    }

    if (parseDuration(s) > 0) {
      return { key: "interval", label: "интервал", className: "badge-auto" };
    }

    if (/^daily at \d{1,2}:\d{2}$/i.test(s)) {
      return { key: "daily", label: "ежедневно", className: "badge-auto" };
    }

    if (/^weekly on [a-z]+ at \d{1,2}:\d{2}$/i.test(s)) {
      return { key: "weekly", label: "еженедельно", className: "badge-auto" };
    }

    if (isCronLike(s)) {
      return { key: "cron", label: "cron", className: "badge-auto" };
    }

    return { key: "custom", label: "custom", className: "badge-auto" };
  }

  function humanSchedule(spec) {
    var s = normalizeSpec(spec);
    var c = classify(s);

    if (!s) return "не задано";
    if (c.key === "manual") return "только вручную";
    if (c.key === "disabled") return "синхронизация отключена";
    if (c.key === "inherit") return "наследуется";

    if (c.key === "interval") {
      return "каждые " + formatDurationMs(parseDuration(s));
    }

    if (c.key === "daily") {
      var d = s.match(/^daily at (\d{1,2}):(\d{2})$/i);
      if (d) return "ежедневно в " + d[1] + ":" + d[2];
    }

    if (c.key === "weekly") {
      var w = s.match(/^weekly on ([a-z]+) at (\d{1,2}):(\d{2})$/i);
      if (w) {
        var days = {
          sunday: "воскресенье",
          monday: "понедельник",
          tuesday: "вторник",
          wednesday: "среда",
          thursday: "четверг",
          friday: "пятница",
          saturday: "суббота"
        };
        return "каждую " + (days[w[1].toLowerCase()] || w[1]) + " в " + w[2] + ":" + w[3];
      }
    }

    if (c.key === "cron") {
      return "cron: " + s;
    }

    return s;
  }

  function nextRun(spec) {
    var s = normalizeSpec(spec);
    if (!s) return null;

    var c = classify(s);
    if (c.key === "manual" || c.key === "disabled" || c.key === "inherit" || c.key === "empty") {
      return null;
    }

    var ms = parseDuration(s);
    if (ms > 0) {
      return new Date(Date.now() + ms);
    }

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

  function serviceHasOverride(row) {
    if (!row) return false;
    return Boolean(row.Overrides || row.overrides || row.HasOverride || row.has_override);
  }

  function getEffective(name) {
    var n = text(name);
    var ln = lower(n);
    var eff = state.schedules && state.schedules.effective ? state.schedules.effective : {};

    if (eff[n]) return normalizeSpec(eff[n]);

    for (var k in eff) {
      if (Object.prototype.hasOwnProperty.call(eff, k) && lower(k) === ln) {
        return normalizeSpec(eff[k]);
      }
    }

    for (var i = 0; i < state.services.length; i++) {
      var row = state.services[i];
      if (lower(serviceName(row)) === ln) {
        return normalizeSpec(serviceSchedule(row));
      }
    }

    return normalizeSpec(state.schedules && state.schedules.global);
  }

  function mikrotikOk() {
    return Boolean(state.status && state.status.mikrotik_ok !== false);
  }

  function statusLabel(status) {
    if (status === "success") return "успех";
    if (status === "error") return "ошибка";
    if (status === "running") return "выполняется";
    return "нет данных";
  }

  function statusBadge(status) {
    var map = {
      success: ["OK", "badge-ok"],
      error: ["Ошибка", "badge-err"],
      running: ["Выполняется", "badge-auto"],
      unknown: ["—", "badge-muted"]
    };
    var item = map[status] || map.unknown;
    return '<span class="dash-badge ' + item[1] + '">' + esc(item[0]) + "</span>";
  }

  function historyTime(rec) {
    if (!rec) return "";
    return rec.time || rec.started_at || rec.finished_at || rec.timestamp || rec.created_at || "";
  }

  function historyService(rec) {
    if (!rec) return "";
    return text(rec.service || rec.name || rec.Service || rec.Name);
  }

  function historyError(rec) {
    if (!rec) return "";
    return text(rec.error || rec.err || rec.message || rec.reason);
  }

  function historyStatus(rec) {
    if (!rec) return "unknown";

    if (historyError(rec)) return "error";

    var st = lower(rec.status || rec.result || rec.state);
    if (st === "error" || st === "failed" || st === "fail") return "error";
    if (st === "ok" || st === "success" || st === "completed") return "success";
    if (st === "running" || st === "started") return "running";

    if (rec.ok === false) return "error";
    if (rec.ok === true) return "success";

    return "unknown";
  }

  function isDryRun(rec) {
    return Boolean(rec && (rec.dry_run || rec.dryrun || rec.dry));
  }

  function numField(obj, names) {
    if (!obj) return 0;
    for (var i = 0; i < names.length; i++) {
      var v = obj[names[i]];
      if (v !== undefined && v !== null) {
        return Number(v) || 0;
      }
    }
    return 0;
  }

  function historyCounts(rec) {
    return {
      added: numField(rec, ["added", "routes_added", "add", "Added"]),
      deleted: numField(rec, ["deleted", "routes_deleted", "del", "Deleted"]),
      unchanged: numField(rec, ["unchanged", "same", "unchanged_count", "Unchanged"])
    };
  }

  function historySubtitle(rec) {
    if (!rec) return "нет данных";

    var parts = [];
    if (isDryRun(rec)) parts.push("dry-run");

    var c = historyCounts(rec);
    if (c.added) parts.push("+" + c.added);
    if (c.deleted) parts.push("-" + c.deleted);
    if (c.unchanged) parts.push("=" + c.unchanged);

    var err = historyError(rec);
    if (err) parts.push(truncate(err, 120));

    return parts.length ? parts.join(" · ") : "без деталей";
  }

  function logLevel(entry) {
    return text(entry && (entry.level || entry.Level)).toUpperCase() || "INFO";
  }

  function isIssueLog(entry) {
    var l = logLevel(entry);
    return l === "ERROR" || l === "WARN";
  }

  function logMessage(entry) {
    return text(entry && (entry.msg || entry.message || entry.Message)) || "сообщение";
  }

  function logService(entry) {
    return text(entry && (entry.service || entry.Service)) || "—";
  }

  function logError(entry) {
    return text(entry && (entry.err || entry.error || entry.Error));
  }

  function logLevelBadge(level) {
    var cls = "badge-muted";
    if (level === "ERROR") cls = "badge-err";
    else if (level === "WARN") cls = "badge-warn";
    else if (level === "INFO") cls = "badge-ok";

    return '<span class="dash-badge ' + cls + '">' + esc(level) + "</span>";
  }

  function summarizeEventData(data) {
    if (data === null || data === undefined) return "";
    if (typeof data !== "object") return truncate(text(data), 140);

    var keys = Object.keys(data).slice(0, 4);
    var parts = [];

    for (var i = 0; i < keys.length; i++) {
      parts.push(keys[i] + "=" + truncate(text(data[keys[i]]), 42));
    }

    return parts.join(", ");
  }

  function buildIndexes() {
    state.degraded = {};

    var deg = state.status && state.status.degraded;
    if (Array.isArray(deg)) {
      each(deg, function (item) {
        var name = "";
        if (typeof item === "string") {
          name = item;
        } else if (item) {
          name = item.service || item.name || item.Service || item.Name;
        }

        name = lower(name);
        if (name) state.degraded[name] = true;
      });
    }

    state.sortedHistory = state.history.slice().sort(function (a, b) {
      var ta = parseTime(historyTime(a));
      var tb = parseTime(historyTime(b));

      if (!ta && !tb) return 0;
      if (!ta) return 1;
      if (!tb) return -1;

      return tb.getTime() - ta.getTime();
    });

    state.historyByService = {};
    each(state.sortedHistory, function (rec) {
      var name = lower(historyService(rec));
      if (name && !state.historyByService[name]) {
        state.historyByService[name] = rec;
      }
    });

    state.issueLogs = state.logs.filter(isIssueLog).slice(0, 80);
  }

  function scheduleCounts() {
    var counts = { auto: 0, manual: 0, disabled: 0, other: 0 };

    each(state.services, function (row) {
      var name = serviceName(row);
      var c = classify(getEffective(name));

      if (c.key === "manual") counts.manual++;
      else if (c.key === "disabled") counts.disabled++;
      else if (c.key === "interval" || c.key === "daily" || c.key === "weekly" || c.key === "cron" || c.key === "custom") counts.auto++;
      else counts.other++;
    });

    return counts;
  }

  function totalRoutes() {
    var sum = 0;
    each(state.services, function (row) {
      sum += serviceRoutes(row);
    });
    return sum;
  }

  function computeNextItems() {
    var items = [];

    each(state.services, function (row) {
      var name = serviceName(row);
      if (!name) return;

      var eff = getEffective(name);
      var next = nextRun(eff);

      if (next) {
        items.push({
          name: name,
          eff: eff,
          next: next
        });
      }
    });

    items.sort(function (a, b) {
      return a.next.getTime() - b.next.getTime() || a.name.localeCompare(b.name);
    });

    return items;
  }

  function serviceHealth(name, hist) {
    if (!state.status) {
      return { label: "нет данных", cls: "badge-muted" };
    }

    if (!mikrotikOk()) {
      return { label: "API ошибка", cls: "badge-err" };
    }

    if (state.degraded[lower(name)]) {
      return { label: "degraded", cls: "badge-warn" };
    }

    var st = hist ? historyStatus(hist) : "unknown";

    if (st === "error") {
      return { label: "ошибка", cls: "badge-err" };
    }

    if (st === "success") {
      return { label: "OK", cls: "badge-ok" };
    }

    return { label: "нет данных", cls: "badge-muted" };
  }

  function healthPriority(health) {
    if (health.cls.indexOf("badge-err") !== -1) return 0;
    if (health.cls.indexOf("badge-warn") !== -1) return 1;
    return 2;
  }

  function serviceComparator(a, b) {
    var pa = healthPriority(a.health);
    var pb = healthPriority(b.health);

    if (pa !== pb) return pa - pb;

    if (a.next && b.next) return a.next.getTime() - b.next.getTime();
    if (a.next) return -1;
    if (b.next) return 1;

    return a.name.localeCompare(b.name);
  }

  function renderKpis() {
    var ok = mikrotikOk();
    var uptime = state.status ? text(state.status.uptime || "—") : "нет данных";

    setKpi(
      "router",
      state.status ? (ok ? "Доступен" : "Недоступен") : "…",
      "uptime: " + uptime,
      state.status ? (ok ? "ok" : "err") : "muted"
    );

    var counts = scheduleCounts();
    setKpi(
      "services",
      String(state.services.length),
      counts.auto + " авто · " + counts.manual + " manual · " + counts.disabled + " disabled",
      "muted"
    );

    var routes = totalRoutes();
    setKpi(
      "routes",
      String(routes),
      routes ? "управляемых IPv4-маршрутов" : "маршрутов пока нет",
      routes ? "ok" : "muted"
    );

    var nextItems = computeNextItems();
    if (nextItems.length) {
      setKpi(
        "next",
        nextItems[0].name,
        formatDateTime(nextItems[0].next) + " · " + relativeNext(nextItems[0].next),
        "ok"
      );
    } else {
      setKpi("next", "—", "нет автоматических запусков", "muted");
    }

    var latest = state.sortedHistory && state.sortedHistory[0];
    if (latest) {
      var st = historyStatus(latest);
      setKpi(
        "last",
        historyService(latest) || "—",
        statusLabel(st) + " · " + relativeTime(historyTime(latest)),
        st === "success" ? "ok" : st === "error" ? "err" : "muted"
      );
    } else {
      setKpi("last", "—", "нет истории синхронизаций", "muted");
    }

    var degCount = Object.keys(state.degraded).length;
    var logCount = state.issueLogs ? state.issueLogs.length : 0;
    var issueTotal = degCount + logCount;

    setKpi(
      "issues",
      String(issueTotal),
      degCount + " degraded · " + logCount + " ошибок/предупреждений",
      issueTotal ? "err" : "ok"
    );
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

    var counts = scheduleCounts();
    var degCount = Object.keys(state.degraded).length;
    var logCount = state.issueLogs ? state.issueLogs.length : 0;
    var latest = state.sortedHistory && state.sortedHistory[0];

    var html = "";

    html += healthRow(
      "RouterOS REST API",
      state.status ? (mikrotikOk() ? "доступен" : "ошибка") : "нет данных",
      state.status ? "uptime: " + text(state.status.uptime || "—") : "запрос /api/v1/status не выполнен",
      state.status ? (mikrotikOk() ? "ok" : "err") : "muted"
    );

    html += healthRow(
      "Сервисы",
      String(state.services.length),
      counts.auto + " авто · " + counts.manual + " manual · " + counts.disabled + " disabled",
      "muted"
    );

    html += healthRow(
      "Degraded",
      String(degCount),
      degCount ? "есть сервисы в degraded-состоянии" : "проблем не обнаружено",
      degCount ? "warn" : "ok"
    );

    html += healthRow(
      "Ошибки и предупреждения",
      String(logCount),
      logCount ? "смотрите вкладку «Проблемы»" : "в последних логах нет WARN/ERROR",
      logCount ? "warn" : "ok"
    );

    html += healthRow(
      "Последняя синхронизация",
      latest ? (historyService(latest) || "—") : "—",
      latest ? statusLabel(historyStatus(latest)) + " · " + relativeTime(historyTime(latest)) : "история пуста",
      latest ? (historyStatus(latest) === "success" ? "ok" : historyStatus(latest) === "error" ? "err" : "muted") : "muted"
    );

    html += healthRow(
      "Глобальное расписание",
      normalizeSpec(state.schedules.global) || "—",
      "значение по умолчанию для сервисов без override",
      "muted"
    );

    els.health.innerHTML = html;
  }

  function renderNext() {
    if (!els.next) return;

    var items = computeNextItems().slice(0, 8);

    if (!items.length) {
      els.next.innerHTML = '<div class="empty-state">Нет автоматических запусков.</div>';
      return;
    }

    var html = "";

    each(items, function (item) {
      html += '<div class="list-row">' +
        '<span class="activity-time">' + esc(formatDateTime(item.next)) + "</span>" +
        '<div class="activity-main">' +
        '<div class="primary-text">' + esc(item.name) + "</div>" +
        '<div class="secondary-text">' + esc(humanSchedule(item.eff)) + "</div>" +
        "</div>" +
        '<div class="activity-right">' +
        '<span class="dash-badge badge-auto">' + esc(relativeNext(item.next)) + "</span>" +
        "</div>" +
        "</div>";
    });

    els.next.innerHTML = html;
  }

  function serviceRowHtml(item) {
    var checked = state.selection[item.key] ? " checked" : "";
    var selectedClass = state.selection[item.key] ? " selected" : "";
    var scheduleClass = classify(item.eff).className;

    var nextHtml = item.next
      ? esc(formatDateTime(item.next)) + '<div class="muted small">' + esc(relativeNext(item.next)) + "</div>"
      : '<span class="muted">—</span>';

    var lastHtml = item.hist
      ? statusBadge(historyStatus(item.hist)) + '<div class="muted small">' + esc(relativeTime(historyTime(item.hist))) + "</div>"
      : '<span class="muted">—</span>';

    return '<tr class="' + selectedClass + '" data-key="' + esc(item.key) + '" data-service="' + esc(item.name) + '">' +
      '<td><input type="checkbox" class="row-select" data-key="' + esc(item.key) + '" data-service="' + esc(item.name) + '"' + checked + "></td>" +
      "<td>" +
      '<div class="svc-name">' +
      "<strong>" + esc(item.name) + "</strong>" +
      (item.hasOverride ? '<span class="dash-badge badge-override">override</span>' : "") +
      "</div>" +
      "</td>" +
      '<td><span class="dash-badge ' + esc(item.health.cls) + '">' + esc(item.health.label) + "</span></td>" +
      "<td>" +
      "<code>" + esc(item.eff || "—") + "</code>" +
      '<div class="muted small">' + esc(humanSchedule(item.eff)) + "</div>" +
      '<div style="margin-top:.25rem"><span class="dash-badge ' + esc(scheduleClass) + '">' + esc(classify(item.eff).label) + "</span></div>" +
      "</td>" +
      "<td>" + nextHtml + "</td>" +
      "<td>" + esc(String(item.routes)) + "</td>" +
      "<td>" + lastHtml + "</td>" +
      "<td>" +
      '<div class="row-actions">' +
      '<button type="button" class="outline" data-action="dry" data-service="' + esc(item.name) + '">Dry</button>' +
      '<button type="button" data-action="sync" data-service="' + esc(item.name) + '">Синхр.</button>' +
      "</div>" +
      "</td>" +
      "</tr>";
  }

  function renderServices() {
    if (!els.servicesBody || !els.servicesEmpty) return;

    var q = lower(els.serviceSearch ? els.serviceSearch.value : "");
    var filter = els.serviceFilter ? els.serviceFilter.value : "all";
    var items = [];

    each(state.services, function (row) {
      var name = serviceName(row);
      if (!name) return;

      var key = lower(name);
      var eff = getEffective(name);
      var cls = classify(eff);
      var next = nextRun(eff);
      var hist = state.historyByService[key];
      var health = serviceHealth(name, hist);

      var item = {
        key: key,
        name: name,
        row: row,
        eff: eff,
        cls: cls,
        next: next,
        hist: hist,
        health: health,
        routes: serviceRoutes(row),
        degraded: Boolean(state.degraded[key]),
        hasOverride: serviceHasOverride(row)
      };

      if (q) {
        var hay = [name, eff, text(hist && historyService(hist))].join(" ").toLowerCase();
        if (hay.indexOf(q) === -1) return;
      }

      if (filter === "auto") {
        if (!(cls.key === "interval" || cls.key === "daily" || cls.key === "weekly" || cls.key === "cron" || cls.key === "custom")) return;
      } else if (filter === "manual") {
        if (cls.key !== "manual") return;
      } else if (filter === "disabled") {
        if (cls.key !== "disabled") return;
      } else if (filter === "problems") {
        if (!item.degraded && item.health.cls.indexOf("badge-err") === -1 && item.health.cls.indexOf("badge-warn") === -1) return;
      } else if (filter === "routes") {
        if (item.routes <= 0) return;
      } else if (filter === "override") {
        if (!item.hasOverride) return;
      }

      items.push(item);
    });

    items.sort(serviceComparator);

    state.visibleItems = items;
    state.visibleKeys = items.map(function (item) {
      return item.key;
    });

    if (!items.length) {
      els.servicesBody.innerHTML = "";
      els.servicesEmpty.hidden = false;
      els.servicesEmpty.textContent = state.services.length
        ? "Ничего не найдено. Измените поиск или фильтр."
        : "Нет сервисов. Добавьте сервис на странице «Сервисы».";
      updateSelectionUI();
      return;
    }

    els.servicesEmpty.hidden = true;

    var html = "";
    each(items, function (item) {
      html += serviceRowHtml(item);
    });

    els.servicesBody.innerHTML = html;
    updateSelectionUI();
  }

  function selectedNames() {
    var arr = [];
    for (var k in state.selection) {
      if (Object.prototype.hasOwnProperty.call(state.selection, k)) {
        arr.push(state.selection[k]);
      }
    }
    return arr;
  }

  function updateSelectionUI() {
    var count = Object.keys(state.selection).length;

    if (els.selectedCount) {
      els.selectedCount.textContent = "выбрано: " + count;
    }

    if (els.actionSyncSelected) {
      els.actionSyncSelected.disabled = count === 0;
    }

    if (els.actionDrySelected) {
      els.actionDrySelected.disabled = count === 0;
    }

    each(els.servicesBody ? els.servicesBody.querySelectorAll("tr[data-key]") : [], function (tr) {
      var key = tr.getAttribute("data-key");
      var isSelected = Boolean(state.selection[key]);

      if (tr.classList) {
        tr.classList.toggle("selected", isSelected);
      }

      var cb = tr.querySelector(".row-select");
      if (cb) cb.checked = isSelected;
    });

    if (els.selectAll) {
      var visibleKeys = state.visibleKeys || [];
      var selectedVisible = 0;

      each(visibleKeys, function (key) {
        if (state.selection[key]) selectedVisible++;
      });

      els.selectAll.checked = visibleKeys.length > 0 && selectedVisible === visibleKeys.length;
      els.selectAll.indeterminate = selectedVisible > 0 && selectedVisible < visibleKeys.length;
    }
  }

  function toggleSelect(key, name, checked) {
    if (checked) {
      state.selection[key] = name;
    } else {
      delete state.selection[key];
    }
    updateSelectionUI();
  }

  function syncActivityHtml() {
    var list = state.sortedHistory.slice(0, 20);

    if (!list.length) {
      return '<div class="empty-state">История синхронизаций пуста.</div>';
    }

    var html = "";

    each(list, function (rec) {
      var st = historyStatus(rec);

      html += '<div class="list-row activity-item">' +
        '<span class="activity-time">' + esc(formatClock(historyTime(rec))) + "</span>" +
        '<div class="activity-main">' +
        '<div class="primary-text">' + esc(historyService(rec) || "—") + "</div>" +
        '<div class="secondary-text">' + esc(historySubtitle(rec)) + "</div>" +
        "</div>" +
        '<div class="activity-right">' + statusBadge(st) + "</div>" +
        "</div>";
    });

    return html;
  }

  function liveActivityHtml() {
    if (!state.live.length) {
      return '<div class="empty-state">Live-событий пока нет. Включите Live и дождитесь событий WebSocket.</div>';
    }

    var html = "";

    each(state.live.slice(0, 50), function (item) {
      html += '<div class="list-row activity-item">' +
        '<span class="activity-time">' + esc(formatClock(item.time)) + "</span>" +
        '<div class="activity-main">' +
        '<div class="primary-text">' + esc(item.event || "event") + "</div>" +
        '<div class="secondary-text">' + esc(summarizeEventData(item.data)) + "</div>" +
        "</div>" +
        '<div class="activity-right">' +
        '<span class="dash-badge badge-auto">WS</span>' +
        "</div>" +
        "</div>";
    });

    return html;
  }

  function issuesActivityHtml() {
    if (!state.issueLogs.length) {
      return '<div class="empty-state">Ошибок и предупреждений в последних логах нет.</div>';
    }

    var html = "";

    each(state.issueLogs.slice(0, 40), function (entry) {
      var level = logLevel(entry);
      var err = logError(entry);
      var subtitle = logService(entry);

      if (err) {
        subtitle += " · " + truncate(err, 140);
      }

      html += '<div class="list-row activity-item">' +
        '<span class="activity-time">' + esc(formatClock(entry.time)) + "</span>" +
        '<div class="activity-main">' +
        '<div class="primary-text">' + esc(logMessage(entry)) + "</div>" +
        '<div class="secondary-text">' + esc(subtitle) + "</div>" +
        "</div>" +
        '<div class="activity-right">' + logLevelBadge(level) + "</div>" +
        "</div>";
    });

    return html;
  }

  function renderActivity() {
    if (!els.activityList) return;

    if (state.activeTab === "live") {
      els.activityList.innerHTML = liveActivityHtml();
    } else if (state.activeTab === "issues") {
      els.activityList.innerHTML = issuesActivityHtml();
    } else {
      els.activityList.innerHTML = syncActivityHtml();
    }
  }

  function renderAll() {
    renderKpis();
    renderHealth();
    renderNext();
    renderServices();
    renderActivity();
  }

  function loadAll(force) {
    if (state.loading && !force) {
      return Promise.resolve();
    }

    setRefreshing(true);

    return Promise.allSettled([
      api("/api/v1/status"),
      api("/api/v1/services"),
      api("/api/v1/schedules"),
      api("/api/v1/history?limit=20"),
      api("/api/v1/logs?limit=120")
    ]).then(function (results) {
      var errors = [];

      if (results[0].status === "fulfilled") {
        state.status = results[0].value || null;
      } else {
        state.status = null;
        errors.push("status");
      }

      if (results[1].status === "fulfilled") {
        var servicesData = results[1].value;
        state.services = servicesData && Array.isArray(servicesData.services) ? servicesData.services : [];
      } else {
        state.services = [];
        errors.push("services");
      }

      if (results[2].status === "fulfilled") {
        state.schedules = results[2].value || { global: "", groups: {}, services: {}, effective: {} };
      } else {
        state.schedules = { global: "", groups: {}, services: {}, effective: {} };
        errors.push("schedules");
      }

      if (results[3].status === "fulfilled") {
        var historyData = results[3].value;
        state.history = Array.isArray(historyData) ? historyData : [];
      } else {
        state.history = [];
        errors.push("history");
      }

      if (results[4].status === "fulfilled") {
        var logsData = results[4].value;
        state.logs = Array.isArray(logsData) ? logsData : [];
      } else {
        state.logs = [];
        errors.push("logs");
      }

      if (errors.length) {
        setAlert(
          errors.length >= 4 ? "error" : "warn",
          "Не удалось загрузить: " + errors.join(", ")
        );
      } else {
        setAlert("", "");
      }

      buildIndexes();
      renderAll();
    }).catch(function (err) {
      setAlert("error", "Ошибка загрузки дашборда: " + text(err && err.message));
    }).then(function () {
      setRefreshing(false);
    });
  }

  function withButton(btn, fn) {
    if (!btn) return Promise.resolve(fn());

    var old = btn.textContent;
    btn.disabled = true;
    btn.textContent = "…";

    return Promise.resolve()
      .then(fn)
      .then(function (value) {
        btn.disabled = false;
        btn.textContent = old;
        return value;
      }, function (err) {
        btn.disabled = false;
        btn.textContent = old;
        throw err;
      });
  }

  function handlePromise(promise) {
    if (promise && promise.catch) {
      promise.catch(function (err) {
        showToast("Ошибка: " + text(err && err.message), true);
      });
    }
  }

  function runBulk(names, dry, btn) {
    var payload = {
      services: names,
      dry_run: dry,
      force: false
    };

    return withButton(btn, function () {
      return api("/api/v1/services/sync", {
        method: "POST",
        body: JSON.stringify(payload)
      }).then(function () {
        var label = dry ? "Dry-run запущен: " : "Синхронизация запущена: ";
        label += names.length ? names.length + " сервисов" : "все сервисы";
        showToast(label);

        setTimeout(function () {
          loadAll(true);
        }, 1500);
      });
    });
  }

  function runOne(name, dry, btn) {
    var path = dry
      ? "/api/v1/services/" + encodeURIComponent(name) + "/dry-run"
      : "/api/v1/services/" + encodeURIComponent(name) + "/sync";

    var body = dry ? "{}" : JSON.stringify({ dry_run: false, force: false });

    return withButton(btn, function () {
      return api(path, {
        method: "POST",
        body: body
      }).then(function (data) {
        var c = historyCounts(data);
        var parts = [];

        if (c.added) parts.push("+" + c.added);
        if (c.deleted) parts.push("-" + c.deleted);
        if (c.unchanged) parts.push("=" + c.unchanged);

        var msg = (dry ? "Dry-run " : "Синхронизация ") + name;
        if (parts.length) msg += ": " + parts.join(" ");

        showToast(msg);

        setTimeout(function () {
          loadAll(true);
        }, 1000);
      });
    });
  }

  function updateLiveButton() {
    if (!els.toggleLive) return;

    els.toggleLive.textContent = state.liveUserEnabled ? "Live: вкл" : "Live: выкл";

    if (els.toggleLive.classList) {
      els.toggleLive.classList.toggle("live-on", state.liveUserEnabled);
    }
  }

  function connectLive() {
    if (state.ws) return;

    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    var url = proto + "//" + location.host + "/api/v1/ws";

    try {
      state.ws = new WebSocket(url);
    } catch (e) {
      showToast("WebSocket недоступен: " + text(e && e.message), true);
      state.liveUserEnabled = false;
      updateLiveButton();
      return;
    }

    state.ws.onopen = function () {
      updateLiveButton();
    };

    state.ws.onmessage = function (ev) {
      try {
        var msg = JSON.parse(ev.data);
        if (!msg || msg.event === "welcome") return;

        state.live.unshift({
          time: msg.time || new Date().toISOString(),
          event: msg.event || "event",
          data: msg.data
        });

        if (state.live.length > 100) {
          state.live.pop();
        }

        if (state.activeTab === "live") {
          renderActivity();
        }
      } catch (e) {
        // Игнорируем некорректные сообщения.
      }
    };

    state.ws.onerror = function () {
      showToast("Ошибка WebSocket", true);
    };

    state.ws.onclose = function () {
      state.ws = null;

      if (state.liveUserEnabled) {
        setTimeout(connectLive, 5000);
      }

      updateLiveButton();
    };
  }

  function disconnectLive() {
    if (state.ws) {
      var ws = state.ws;
      state.ws = null;

      ws.onopen = null;
      ws.onmessage = null;
      ws.onerror = null;
      ws.onclose = null;

      try {
        ws.close();
      } catch (e) {}
    }

    updateLiveButton();
  }

  function scheduleAuto() {
    if (state.autoTimer) {
      clearInterval(state.autoTimer);
      state.autoTimer = null;
    }

    if (els.autoRefresh && els.autoRefresh.checked) {
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
        handlePromise(loadAll(true));
      });
    }

    if (els.toggleLive) {
      els.toggleLive.addEventListener("click", function () {
        state.liveUserEnabled = !state.liveUserEnabled;

        if (state.liveUserEnabled) {
          connectLive();
        } else {
          disconnectLive();
        }

        updateLiveButton();
      });
    }

    if (els.autoRefresh) {
      els.autoRefresh.addEventListener("change", scheduleAuto);
    }

    if (els.actionSyncAll) {
      els.actionSyncAll.addEventListener("click", function () {
        handlePromise(runBulk([], false, els.actionSyncAll));
      });
    }

    if (els.actionDryAll) {
      els.actionDryAll.addEventListener("click", function () {
        handlePromise(runBulk([], true, els.actionDryAll));
      });
    }

    if (els.actionSyncSelected) {
      els.actionSyncSelected.addEventListener("click", function () {
        var names = selectedNames();
        if (!names.length) return;
        handlePromise(runBulk(names, false, els.actionSyncSelected));
      });
    }

    if (els.actionDrySelected) {
      els.actionDrySelected.addEventListener("click", function () {
        var names = selectedNames();
        if (!names.length) return;
        handlePromise(runBulk(names, true, els.actionDrySelected));
      });
    }

    if (els.serviceSearch) {
      var searchTimer = null;
      els.serviceSearch.addEventListener("input", function () {
        clearTimeout(searchTimer);
        searchTimer = setTimeout(renderServices, 180);
      });
    }

    if (els.serviceFilter) {
      els.serviceFilter.addEventListener("change", renderServices);
    }

    if (els.selectAll) {
      els.selectAll.addEventListener("change", function () {
        var checked = els.selectAll.checked;

        each(state.visibleItems, function (item) {
          if (checked) {
            state.selection[item.key] = item.name;
          } else {
            delete state.selection[item.key];
          }
        });

        renderServices();
      });
    }

    if (els.servicesBody) {
      els.servicesBody.addEventListener("change", function (e) {
        var cb = e.target;
        if (!cb || !cb.classList || !cb.classList.contains("row-select")) return;

        toggleSelect(cb.getAttribute("data-key"), cb.getAttribute("data-service"), cb.checked);
      });

      els.servicesBody.addEventListener("click", function (e) {
        var btn = closest(e.target, "button[data-action]");
        if (!btn) return;

        var name = btn.getAttribute("data-service");
        var action = btn.getAttribute("data-action");

        if (!name) return;

        if (action === "sync") {
          handlePromise(runOne(name, false, btn));
        } else if (action === "dry") {
          handlePromise(runOne(name, true, btn));
        }
      });
    }

    each(els.tabs, function (tab) {
      tab.addEventListener("click", function () {
        state.activeTab = tab.getAttribute("data-tab") || "sync";

        each(els.tabs, function (t) {
          if (t.classList) {
            t.classList.toggle("active", t === tab);
          }
        });

        renderActivity();
      });
    });

    document.addEventListener("keydown", function (e) {
      var tag = document.activeElement && document.activeElement.tagName;
      var typing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";

      if (e.key === "/" && !typing && els.serviceSearch) {
        e.preventDefault();
        els.serviceSearch.focus();
      }

      if ((e.ctrlKey || e.metaKey) && e.key === "r") {
        e.preventDefault();
        handlePromise(loadAll(true));
      }
    });
  }

  function init() {
    bindEvents();
    updateLiveButton();
    loadAll(true);
    scheduleAuto();
  }

  init();
})();
