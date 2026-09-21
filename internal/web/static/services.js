(function () {
  "use strict";

  var page = document.getElementById("services-page");
  if (!page) return;

  function byId(id) {
    return document.getElementById(id);
  }

  function qs(sel, root) {
    return (root || document).querySelector(sel);
  }

  function qsa(sel, root) {
    return (root || document).querySelectorAll(sel);
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

  function trim(value) {
    return text(value).trim();
  }

  function lower(value) {
    return trim(value).toLowerCase();
  }

  function pad(n) {
    return n < 10 ? "0" + n : String(n);
  }

  function truncate(value, max) {
    var s = text(value);
    if (s.length <= max) return s;
    return s.slice(0, max - 1) + "…";
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
    alert: byId("svc-alert"),
    refresh: byId("svc-refresh"),
    auto: byId("svc-auto-refresh"),
    search: byId("svc-search"),
    filter: byId("svc-filter"),
    sort: byId("svc-sort"),
    selectAll: byId("svc-select-all"),
    selectedInfo: byId("svc-selected-info"),
    bulkSync: byId("svc-bulk-sync"),
    bulkDry: byId("svc-bulk-dry"),
    bulkClear: byId("svc-bulk-clear"),
    addForm: byId("svc-add-form"),
    addInput: byId("svc-add-name"),
    addBtn: byId("svc-add-btn"),
    body: byId("services-body"),
    empty: byId("services-empty"),
    drawer: byId("svc-drawer"),
    drawerTitle: byId("svc-drawer-title"),
    drawerSubtitle: byId("svc-drawer-subtitle"),
    drawerBody: byId("svc-drawer-body"),
    drawerSync: byId("svc-drawer-sync"),
    drawerDry: byId("svc-drawer-dry"),
    drawerDelete: byId("svc-drawer-delete"),
    deleteModal: byId("svc-delete-modal"),
    deleteName: byId("svc-delete-name"),
    deleteKeep: byId("svc-delete-keep"),
    deletePurge: byId("svc-delete-purge"),
    deleteCancel: byId("svc-delete-cancel")
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
    items: [],
    visibleKeys: [],
    historyByService: {},
    allHistorySorted: [],
    errorCountByService: {},
    warnCountByService: {},
    degraded: {},
    selection: {},
    loading: false,
    autoTimer: null,
    details: {
      open: false,
      name: null,
      tab: "overview",
      loading: false,
      history: [],
      logs: []
    },
    delete: {
      open: false,
      name: null
    }
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
      els.alert.className = "svc-alert";
      els.alert.textContent = "";
      return;
    }

    els.alert.hidden = false;
    els.alert.className = "svc-alert " + (kind || "warn");
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

  function setKpi(valueId, noteId, value, note, stateName) {
    var v = byId(valueId);
    var n = byId(noteId);

    if (v) v.textContent = text(value);
    if (n) n.textContent = text(note);

    var card = v && v.closest ? v.closest(".svc-kpi-card") : null;
    if (card) {
      card.className = "svc-kpi-card" + (stateName ? " svc-state-" + stateName : "");
    }
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

  function formatDateTime(value) {
    var d = parseTime(value);
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
      return { key: "empty", label: "не задано", className: "svc-badge-muted" };
    }

    var lowerSpec = s.toLowerCase();

    if (lowerSpec === "manual") {
      return { key: "manual", label: "вручную", className: "svc-badge-manual" };
    }

    if (lowerSpec === "disabled") {
      return { key: "disabled", label: "отключено", className: "svc-badge-disabled" };
    }

    if (lowerSpec === "inherit") {
      return { key: "inherit", label: "наследуется", className: "svc-badge-muted" };
    }

    if (parseDuration(s) > 0) {
      return { key: "interval", label: "интервал", className: "svc-badge-auto" };
    }

    if (/^daily at \d{1,2}:\d{2}$/i.test(s)) {
      return { key: "daily", label: "ежедневно", className: "svc-badge-auto" };
    }

    if (/^weekly on [a-z]+ at \d{1,2}:\d{2}$/i.test(s)) {
      return { key: "weekly", label: "еженедельно", className: "svc-badge-auto" };
    }

    if (isCronLike(s)) {
      return { key: "cron", label: "cron", className: "svc-badge-auto" };
    }

    return { key: "custom", label: "custom", className: "svc-badge-auto" };
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

  function getScheduleOverrideSpec(name) {
    var n = text(name);
    var ln = lower(n);
    var svcs = state.schedules && state.schedules.services;

    if (!svcs || typeof svcs !== "object") return "";

    var direct = svcs[n] || svcs[ln];
    if (direct !== undefined) {
      if (typeof direct === "string") return normalizeSpec(direct);
      return normalizeSpec(direct.schedule || direct.Schedule || "");
    }

    for (var k in svcs) {
      if (!Object.prototype.hasOwnProperty.call(svcs, k)) continue;
      if (lower(k) !== ln) continue;

      var val = svcs[k];
      if (typeof val === "string") return normalizeSpec(val);
      return normalizeSpec(val.schedule || val.Schedule || "");
    }

    return "";
  }

  function findGroup(name) {
    var n = lower(name);
    var groups = state.schedules && state.schedules.groups;

    if (!groups) return "";

    if (Array.isArray(groups)) {
      for (var i = 0; i < groups.length; i++) {
        var g = groups[i];
        var services = g.services || g.Services;
        if (Array.isArray(services)) {
          for (var j = 0; j < services.length; j++) {
            if (lower(services[j]) === n) {
              return text(g.name || g.Name || "");
            }
          }
        }
      }
      return "";
    }

    for (var key in groups) {
      if (!Object.prototype.hasOwnProperty.call(groups, key)) continue;

      var val = groups[key];
      var list = val && (val.services || val.Services);

      if (Array.isArray(list)) {
        for (var x = 0; x < list.length; x++) {
          if (lower(list[x]) === n) return key;
        }
      }
    }

    return "";
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
      var row = state.services[i];
      if (lower(serviceName(row)) === ln) {
        return normalizeSpec(serviceSchedule(row));
      }
    }

    return normalizeSpec(state.schedules && state.schedules.global);
  }

  function sourceFor(name) {
    var overrideSpec = getScheduleOverrideSpec(name);
    if (overrideSpec) return "service";

    var group = findGroup(name);
    if (group) return "group";

    return "global";
  }

  function sourceLabel(source) {
    if (source === "service") return "переопределение сервиса";
    if (source === "group") return "группа";
    return "глобальное";
  }

  function sourceBadge(source) {
    if (source === "service") return '<span class="svc-badge svc-badge-override">сервис</span>';
    if (source === "group") return '<span class="svc-badge svc-badge-group">группа</span>';
    return '<span class="svc-badge svc-badge-ok">глобально</span>';
  }

  function historyTime(rec) {
    if (!rec) return "";
    return rec.time || rec.started_at || rec.finished_at || rec.timestamp || rec.created_at ||
      rec.Time || rec.StartedAt || rec.FinishedAt || "";
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

  function isDryRun(rec) {
    return Boolean(rec && (rec.dry_run || rec.dryrun || rec.dry || rec.DryRun));
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
    if (err) parts.push(truncate(err, 140));

    return parts.length ? parts.join(" · ") : "без деталей";
  }

  function statusBadge(status) {
    var map = {
      success: ["OK", "svc-badge-ok"],
      error: ["Ошибка", "svc-badge-err"],
      warning: ["Warn", "svc-badge-warn"],
      running: ["Выполняется", "svc-badge-auto"],
      unknown: ["—", "svc-badge-muted"]
    };
    var item = map[status] || map.unknown;
    return '<span class="svc-badge ' + item[1] + '">' + esc(item[0]) + "</span>";
  }

  function logLevel(entry) {
    return text(entry && (entry.level || entry.Level)).toUpperCase() || "INFO";
  }

  function logMessage(entry) {
    return text(entry && (entry.msg || entry.message || entry.Message)) || "сообщение";
  }

  function logService(entry) {
    return text(entry && (entry.service || entry.Service)) || "";
  }

  function logError(entry) {
    return text(entry && (entry.err || entry.error || entry.Error));
  }

  function logLevelBadge(level) {
    var cls = "svc-badge-muted";
    if (level === "ERROR") cls = "svc-badge-err";
    else if (level === "WARN") cls = "svc-badge-warn";
    else if (level === "INFO") cls = "svc-badge-ok";

    return '<span class="svc-badge ' + cls + '">' + esc(level) + "</span>";
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

    state.allHistorySorted = state.history.slice().sort(function (a, b) {
      var ta = parseTime(historyTime(a));
      var tb = parseTime(historyTime(b));

      if (!ta && !tb) return 0;
      if (!ta) return 1;
      if (!tb) return -1;

      return tb.getTime() - ta.getTime();
    });

    state.historyByService = {};
    each(state.allHistorySorted, function (rec) {
      var name = lower(historyService(rec));
      if (name && !state.historyByService[name]) {
        state.historyByService[name] = rec;
      }
    });

    state.errorCountByService = {};
    state.warnCountByService = {};

    each(state.logs, function (entry) {
      var name = lower(logService(entry));
      if (!name) return;

      var level = logLevel(entry);
      if (level === "ERROR") {
        state.errorCountByService[name] = (state.errorCountByService[name] || 0) + 1;
      } else if (level === "WARN") {
        state.warnCountByService[name] = (state.warnCountByService[name] || 0) + 1;
      }
    });
  }

  function computeHealth(name, last, errorCount, warnCount) {
    if (!state.status) {
      return { label: "нет данных", cls: "svc-badge-muted", priority: 3 };
    }

    var key = lower(name);

    if (state.degraded[key]) {
      return { label: "degraded", cls: "svc-badge-warn", priority: 1 };
    }

    var lastStatus = last ? historyStatus(last) : "unknown";

    if (errorCount > 0 || lastStatus === "error") {
      return {
        label: errorCount > 1 ? errorCount + " ошибки" : "ошибка",
        cls: "svc-badge-err",
        priority: 0
      };
    }

    if (warnCount > 0 || lastStatus === "warning") {
      return {
        label: warnCount > 1 ? warnCount + " warn" : "предупреждение",
        cls: "svc-badge-warn",
        priority: 1
      };
    }

    if (lastStatus === "success") {
      return { label: "OK", cls: "svc-badge-ok", priority: 2 };
    }

    if (lastStatus === "running") {
      return { label: "выполняется", cls: "svc-badge-auto", priority: 2 };
    }

    return { label: "нет данных", cls: "svc-badge-muted", priority: 3 };
  }

  function buildItems() {
    state.items = [];
    var validSelection = {};

    each(state.services, function (row) {
      var name = serviceName(row);
      if (!name) return;

      var key = lower(name);
      var effective = getEffective(name);
      var cls = classify(effective);
      var next = nextRun(effective);
      var last = state.historyByService[key];
      var errorCount = state.errorCountByService[key] || 0;
      var warnCount = state.warnCountByService[key] || 0;
      var health = computeHealth(name, last, errorCount, warnCount);
      var group = findGroup(name);
      var source = sourceFor(name);
      var scheduleOverride = !!getScheduleOverrideSpec(name);
      var configOverride = serviceHasOverride(row);

      if (state.selection[key]) {
        validSelection[key] = name;
      }

      state.items.push({
        key: key,
        name: name,
        row: row,
        routes: serviceRoutes(row),
        effective: effective,
        cls: cls,
        next: next,
        last: last,
        health: health,
        errorCount: errorCount,
        warnCount: warnCount,
        group: group,
        source: source,
        scheduleOverride: scheduleOverride,
        configOverride: configOverride
      });
    });

    state.selection = validSelection;
  }

  function totalRoutes() {
    var sum = 0;
    each(state.items, function (item) {
      sum += item.routes;
    });
    return sum;
  }

  function renderKpis() {
    var total = state.items.length;
    var withRoutes = 0;
    var auto = 0;
    var problems = 0;

    each(state.items, function (item) {
      if (item.routes > 0) withRoutes++;

      if (
        item.cls.key === "interval" ||
        item.cls.key === "daily" ||
        item.cls.key === "weekly" ||
        item.cls.key === "cron" ||
        item.cls.key === "custom"
      ) {
        auto++;
      }

      if (item.health.priority <= 1) problems++;
    });

    var selected = Object.keys(state.selection).length;
    var latest = state.allHistorySorted && state.allHistorySorted[0];

    setKpi(
      "svc-total-value",
      "svc-total-note",
      String(total),
      withRoutes + " с маршрутами",
      total ? "muted" : "muted"
    );

    setKpi(
      "svc-routes-value",
      "svc-routes-note",
      String(totalRoutes()),
      totalRoutes() ? "управляемых IPv4" : "маршрутов пока нет",
      totalRoutes() ? "ok" : "muted"
    );

    setKpi(
      "svc-auto-value",
      "svc-auto-note",
      String(auto),
      (total - auto) + " manual/disabled/нет",
      auto ? "ok" : "muted"
    );

    setKpi(
      "svc-problems-value",
      "svc-problems-note",
      String(problems),
      problems ? "ошибки, warnings, degraded" : "проблем не обнаружено",
      problems ? "err" : "ok"
    );

    if (latest) {
      var st = historyStatus(latest);
      setKpi(
        "svc-last-value",
        "svc-last-note",
        historyService(latest) || "—",
        st + " · " + relativeTime(historyTime(latest)),
        st === "success" ? "ok" : st === "error" ? "err" : st === "warning" ? "warn" : "muted"
      );
    } else {
      setKpi("svc-last-value", "svc-last-note", "—", "история пуста", "muted");
    }

    setKpi(
      "svc-selected-value",
      "svc-selected-note",
      String(selected),
      selected ? "готово к массовым действиям" : "отметьте сервисы для действий",
      selected ? "ok" : "muted"
    );
  }

  function rowHtml(item) {
    var checked = state.selection[item.key] ? " checked" : "";
    var selectedClass = state.selection[item.key] ? " svc-selected" : "";

    var nextHtml = item.next
      ? esc(formatDateTime(item.next)) + '<div class="svc-muted svc-small">' + esc(relativeNext(item.next)) + "</div>"
      : '<span class="svc-muted">—</span>';

    var lastHtml = item.last
      ? statusBadge(historyStatus(item.last)) + '<div class="svc-muted svc-small">' + esc(relativeTime(historyTime(item.last))) + "</div>"
      : '<span class="svc-muted">—</span>';

    var badges = "";
    if (item.configOverride) badges += '<span class="svc-badge svc-badge-override">override</span>';
    if (item.group) badges += '<span class="svc-badge svc-badge-group">' + esc(item.group) + "</span>";
    if (item.errorCount) badges += '<span class="svc-badge svc-badge-err">' + esc(item.errorCount) + " err</span>";
    if (item.warnCount) badges += '<span class="svc-badge svc-badge-warn">' + esc(item.warnCount) + " warn</span>";

    return '<tr class="' + selectedClass + '" data-key="' + esc(item.key) + '" data-service="' + esc(item.name) + '">' +
      '<td data-label="Выбор"><input type="checkbox" class="svc-row-check" data-key="' + esc(item.key) + '" data-service="' + esc(item.name) + '"' + checked + "></td>" +
      '<td data-label="Сервис">' +
        '<div class="svc-name"><strong>' + esc(item.name) + "</strong>" + badges + "</div>" +
      "</td>" +
      '<td data-label="Состояние"><span class="svc-badge ' + esc(item.health.cls) + '">' + esc(item.health.label) + "</span></td>" +
      '<td data-label="Расписание">' +
        "<code>" + esc(item.effective || "—") + "</code>" +
        '<div class="svc-muted svc-small">' + esc(humanSchedule(item.effective)) + "</div>" +
        '<div style="margin-top:.25rem"><span class="svc-badge ' + esc(item.cls.className) + '">' + esc(item.cls.label) + "</span></div>" +
      "</td>" +
      '<td data-label="Ближайший запуск">' + nextHtml + "</td>" +
      '<td data-label="Маршрутов">' + esc(String(item.routes)) + "</td>" +
      '<td data-label="Последняя синхр.">' + lastHtml + "</td>" +
      '<td data-label="Действия">' +
        '<div class="svc-actions">' +
          '<button type="button" class="svc-outline" data-action="details" data-service="' + esc(item.name) + '">Детали</button>' +
          '<button type="button" data-action="dry" data-service="' + esc(item.name) + '">Dry</button>' +
          '<button type="button" data-action="sync" data-service="' + esc(item.name) + '">Синхр.</button>' +
          '<button type="button" class="svc-danger" data-action="delete" data-service="' + esc(item.name) + '">Удалить</button>' +
        "</div>" +
      "</td>" +
      "</tr>";
  }

  function filterItem(item) {
    var q = lower(els.search ? els.search.value : "");
    var mode = els.filter ? els.filter.value : "all";

    if (q) {
      var hay = [
        item.name,
        item.effective,
        item.group,
        item.health.label,
        item.cls.label
      ].join(" ").toLowerCase();

      if (hay.indexOf(q) === -1) return false;
    }

    if (mode === "auto") {
      return (
        item.cls.key === "interval" ||
        item.cls.key === "daily" ||
        item.cls.key === "weekly" ||
        item.cls.key === "cron" ||
        item.cls.key === "custom"
      );
    }

    if (mode === "manual") return item.cls.key === "manual";
    if (mode === "disabled") return item.cls.key === "disabled";
    if (mode === "routes") return item.routes > 0;
    if (mode === "problems") return item.health.priority <= 1;
    if (mode === "override") return item.configOverride || item.scheduleOverride;

    return true;
  }

  function sortItems(items) {
    var mode = els.sort ? els.sort.value : "name";
    var sorted = items.slice();

    sorted.sort(function (a, b) {
      if (mode === "routes") {
        return (b.routes - a.routes) || a.name.localeCompare(b.name);
      }

      if (mode === "next") {
        if (a.next && b.next) return a.next.getTime() - b.next.getTime();
        if (a.next) return -1;
        if (b.next) return 1;
        return a.name.localeCompare(b.name);
      }

      if (mode === "last") {
        var at = parseTime(historyTime(a.last));
        var bt = parseTime(historyTime(b.last));

        if (at && bt) return bt.getTime() - at.getTime();
        if (at) return -1;
        if (bt) return 1;
        return a.name.localeCompare(b.name);
      }

      if (mode === "status") {
        if (a.health.priority !== b.health.priority) {
          return a.health.priority - b.health.priority;
        }
        return a.name.localeCompare(b.name);
      }

      return a.name.localeCompare(b.name);
    });

    return sorted;
  }

  function renderTable() {
    if (!els.body || !els.empty) return;

    var visible = state.items.filter(filterItem);
    visible = sortItems(visible);

    state.visibleKeys = visible.map(function (item) {
      return item.key;
    });

    if (!visible.length) {
      els.body.innerHTML = "";
      els.empty.hidden = false;
      els.empty.textContent = state.items.length
        ? "Ничего не найдено. Измените поиск или фильтр."
        : "Нет сервисов. Добавьте первый сервис выше.";
      updateSelectionUI();
      return;
    }

    els.empty.hidden = true;

    var html = "";
    each(visible, function (item) {
      html += rowHtml(item);
    });

    els.body.innerHTML = html;
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

    if (els.selectedInfo) {
      els.selectedInfo.textContent = "выбрано: " + count;
    }

    if (els.bulkSync) {
      els.bulkSync.disabled = count === 0;
    }

    if (els.bulkDry) {
      els.bulkDry.disabled = count === 0;
    }

    each(els.body ? els.body.querySelectorAll("tr[data-key]") : [], function (tr) {
      var key = tr.getAttribute("data-key");
      var isSelected = Boolean(state.selection[key]);

      if (tr.classList) {
        tr.classList.toggle("svc-selected", isSelected);
      }

      var cb = tr.querySelector(".svc-row-check");
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

    setKpi(
      "svc-selected-value",
      "svc-selected-note",
      String(count),
      count ? "готово к массовым действиям" : "отметьте сервисы для действий",
      count ? "ok" : "muted"
    );
  }

  function toggleSelect(key, name, checked) {
    if (checked) {
      state.selection[key] = name;
    } else {
      delete state.selection[key];
    }
    updateSelectionUI();
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

  function loadAll(force) {
    if (state.loading && !force) {
      return Promise.resolve();
    }

    setRefreshing(true);

    return Promise.allSettled([
      api("/api/v1/status"),
      api("/api/v1/services"),
      api("/api/v1/schedules"),
      api("/api/v1/history?limit=120"),
      api("/api/v1/logs?limit=250")
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
      buildItems();
      renderKpis();
      renderTable();

      if (state.details.open) {
        renderDetails();
      }
    }).catch(function (err) {
      setAlert("error", "Ошибка загрузки страницы: " + text(err && err.message));
    }).then(function () {
      setRefreshing(false);
    });
  }

  function addService(name) {
    return api("/api/v1/services", {
      method: "POST",
      body: JSON.stringify({ name: name })
    }).then(function () {
      showToast("Сервис добавлен: " + name);
      return loadAll(true);
    });
  }

  function syncOne(name, dry, btn) {
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

  function bulkAction(names, dry, btn) {
    if (!names.length) return Promise.resolve();

    return withButton(btn, function () {
      return api("/api/v1/services/sync", {
        method: "POST",
        body: JSON.stringify({
          services: names,
          dry_run: dry,
          force: false
        })
      }).then(function () {
        showToast((dry ? "Dry-run запущен: " : "Синхронизация запущена: ") + names.length + " сервисов");
        setTimeout(function () {
          loadAll(true);
        }, 1500);
      });
    });
  }

  function deleteService(name, purge) {
    return api("/api/v1/services/" + encodeURIComponent(name) + "?purge=" + (purge ? "true" : "false"), {
      method: "DELETE"
    }).then(function () {
      showToast("Сервис удалён: " + name + (purge ? " вместе с маршрутами" : ""));
      delete state.selection[lower(name)];
      closeDelete();
      closeDrawer();
      return loadAll(true);
    });
  }

  function openDrawer(name) {
    state.details.name = name;
    state.details.tab = "overview";
    state.details.loading = true;
    state.details.history = [];
    state.details.logs = [];

    if (els.drawer) {
      els.drawer.hidden = false;
      els.drawer.setAttribute("aria-hidden", "false");
    }

    document.body.classList.add("svc-no-scroll");
    renderDetails();
    loadDetails();
  }

  function closeDrawer() {
    state.details.open = false;
    state.details.name = null;

    if (els.drawer) {
      els.drawer.hidden = true;
      els.drawer.setAttribute("aria-hidden", "true");
    }

    if (!state.delete.open) {
      document.body.classList.remove("svc-no-scroll");
    }
  }

  function isDrawerOpen() {
    return Boolean(els.drawer && !els.drawer.hidden);
  }

  function openDelete(name) {
    state.delete.name = name;
    state.delete.open = true;

    if (els.deleteName) {
      els.deleteName.textContent = name;
    }

    if (els.deleteModal) {
      els.deleteModal.hidden = false;
    }

    document.body.classList.add("svc-no-scroll");
  }

  function closeDelete() {
    state.delete.open = false;
    state.delete.name = null;

    if (els.deleteModal) {
      els.deleteModal.hidden = true;
    }

    if (!isDrawerOpen()) {
      document.body.classList.remove("svc-no-scroll");
    }
  }

  function findItem(name) {
    var key = lower(name);
    for (var i = 0; i < state.items.length; i++) {
      if (state.items[i].key === key) return state.items[i];
    }
    return null;
  }

  function detailItem(label, value, note, stateName) {
    return '<div class="svc-detail-item ' + esc(stateName || "muted") + '">' +
      '<span class="svc-detail-label">' + esc(label) + "</span>" +
      '<span class="svc-detail-value">' + value + "</span>" +
      (note ? '<span class="svc-detail-note">' + esc(note) + "</span>" : "") +
      "</div>";
  }

  function renderOverview() {
    var item = findItem(state.details.name);
    if (!item) {
      return '<div class="svc-empty">Сервис не найден в текущей выборке.</div>';
    }

    var html = '<div class="svc-detail-grid">';

    html += detailItem(
      "Состояние",
      '<span class="svc-badge ' + esc(item.health.cls) + '">' + esc(item.health.label) + "</span>",
      item.errorCount ? item.errorCount + " ошибок в логах" : item.warnCount ? item.warnCount + " предупреждений" : "по истории и логам",
      item.health.priority === 0 ? "err" : item.health.priority === 1 ? "warn" : item.health.priority === 2 ? "ok" : "muted"
    );

    html += detailItem(
      "Маршруты",
      esc(String(item.routes)),
      item.routes ? "управляемые IPv4" : "нет активных маршрутов",
      item.routes ? "ok" : "muted"
    );

    html += detailItem(
      "Расписание",
      "<code>" + esc(item.effective || "—") + "</code>",
      humanSchedule(item.effective),
      "muted"
    );

    html += detailItem(
      "Источник",
      sourceBadge(item.source),
      sourceLabel(item.source),
      item.source === "service" ? "warn" : item.source === "group" ? "muted" : "ok"
    );

    html += detailItem(
      "Ближайший запуск",
      item.next ? esc(formatDateTime(item.next)) : "—",
      item.next ? relativeNext(item.next) : "нет автоматического запуска",
      item.next ? "ok" : "muted"
    );

    html += detailItem(
      "Последняя синхронизация",
      item.last ? statusBadge(historyStatus(item.last)) : "—",
      item.last ? historySubtitle(item.last) + " · " + relativeTime(historyTime(item.last)) : "нет истории",
      item.last ? (historyStatus(item.last) === "success" ? "ok" : historyStatus(item.last) === "error" ? "err" : "warn") : "muted"
    );

    html += detailItem(
      "Override",
      item.configOverride ? '<span class="svc-badge svc-badge-override">есть</span>' : '<span class="svc-badge svc-badge-muted">нет</span>',
      item.scheduleOverride ? "есть override расписания" : "наследуется из группы/глобально",
      item.configOverride ? "warn" : "muted"
    );

    html += detailItem(
      "Группа",
      item.group ? '<span class="svc-badge svc-badge-group">' + esc(item.group) + "</span>" : "—",
      item.group ? "сервис входит в группу расписаний" : "не входит в группу",
      item.group ? "muted" : "muted"
    );

    html += "</div>";

    return html;
  }

  function renderHistoryList(list) {
    if (!list || !list.length) {
      return '<div class="svc-empty">История синхронизаций пуста.</div>';
    }

    var html = '<div class="svc-list">';

    each(list, function (rec) {
      html += '<div class="svc-list-row">' +
        '<span class="svc-time">' + esc(formatClock(historyTime(rec))) + "</span>" +
        '<div>' +
          '<div class="svc-primary-text">' + esc(historyService(rec) || "—") + "</div>" +
          '<div class="svc-secondary-text">' + esc(historySubtitle(rec)) + "</div>" +
        "</div>" +
        '<div class="svc-right">' + statusBadge(historyStatus(rec)) + "</div>" +
        "</div>";
    });

    html += "</div>";
    return html;
  }

  function renderLogsList(list) {
    if (!list || !list.length) {
      return '<div class="svc-empty">Логи для этого сервиса не найдены.</div>';
    }

    var html = '<div class="svc-list">';

    each(list, function (entry) {
      var level = logLevel(entry);
      var err = logError(entry);
      var subtitle = logService(entry) || state.details.name;

      if (err) {
        subtitle += " · " + truncate(err, 140);
      }

      html += '<div class="svc-list-row">' +
        '<span class="svc-time">' + esc(formatClock(entry.time)) + "</span>" +
        '<div>' +
          '<div class="svc-primary-text">' + esc(logMessage(entry)) + "</div>" +
          '<div class="svc-secondary-text">' + esc(subtitle) + "</div>" +
        "</div>" +
        '<div class="svc-right">' + logLevelBadge(level) + "</div>" +
        "</div>";
    });

    html += "</div>";
    return html;
  }

  function renderDetails() {
    if (!state.details.name || !els.drawerBody) return;

    var item = findItem(state.details.name);

    if (els.drawerTitle) {
      els.drawerTitle.textContent = state.details.name;
    }

    if (els.drawerSubtitle) {
      els.drawerSubtitle.textContent = item
        ? item.health.label + " · " + humanSchedule(item.effective)
        : "нет данных";
    }

    each(qsa(".svc-tab"), function (tab) {
      if (tab.classList) {
        tab.classList.toggle("active", tab.getAttribute("data-svc-tab") === state.details.tab);
      }
    });

    if (state.details.loading) {
      els.drawerBody.innerHTML = '<div class="svc-empty">Загрузка деталей…</div>';
      return;
    }

    if (state.details.tab === "history") {
      els.drawerBody.innerHTML = renderHistoryList(state.details.history);
    } else if (state.details.tab === "logs") {
      els.drawerBody.innerHTML = renderLogsList(state.details.logs);
    } else {
      els.drawerBody.innerHTML = renderOverview();
    }
  }

  function loadDetails() {
    var name = state.details.name;
    if (!name) return;

    state.details.loading = true;
    renderDetails();

    Promise.allSettled([
      api("/api/v1/history?service=" + encodeURIComponent(name) + "&limit=30"),
      api("/api/v1/logs?service=" + encodeURIComponent(name) + "&limit=80")
    ]).then(function (results) {
      if (state.details.name !== name) return;

      state.details.history = results[0].status === "fulfilled" && Array.isArray(results[0].value)
        ? results[0].value
        : [];

      state.details.logs = results[1].status === "fulfilled" && Array.isArray(results[1].value)
        ? results[1].value
        : [];

      state.details.loading = false;
      renderDetails();
    });
  }

  function scheduleAuto() {
    if (state.autoTimer) {
      clearInterval(state.autoTimer);
      state.autoTimer = null;
    }

    if (els.auto && els.auto.checked) {
      state.autoTimer = setInterval(function () {
        if (
          document.visibilityState === "visible" &&
          !state.loading &&
          !state.details.open &&
          !state.delete.open
        ) {
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

    if (els.auto) {
      els.auto.addEventListener("change", scheduleAuto);
    }

    if (els.search) {
      var searchTimer = null;
      els.search.addEventListener("input", function () {
        clearTimeout(searchTimer);
        searchTimer = setTimeout(renderTable, 180);
      });
    }

    if (els.filter) {
      els.filter.addEventListener("change", renderTable);
    }

    if (els.sort) {
      els.sort.addEventListener("change", renderTable);
    }

    if (els.selectAll) {
      els.selectAll.addEventListener("change", function () {
        var checked = els.selectAll.checked;

        each(state.visibleKeys, function (key) {
          var item = null;
          for (var i = 0; i < state.items.length; i++) {
            if (state.items[i].key === key) {
              item = state.items[i];
              break;
            }
          }

          if (!item) return;

          if (checked) {
            state.selection[key] = item.name;
          } else {
            delete state.selection[key];
          }
        });

        updateSelectionUI();
      });
    }

    if (els.bulkClear) {
      els.bulkClear.addEventListener("click", function () {
        state.selection = {};
        updateSelectionUI();
      });
    }

    if (els.bulkSync) {
      els.bulkSync.addEventListener("click", function () {
        handlePromise(bulkAction(selectedNames(), false, els.bulkSync));
      });
    }

    if (els.bulkDry) {
      els.bulkDry.addEventListener("click", function () {
        handlePromise(bulkAction(selectedNames(), true, els.bulkDry));
      });
    }

    if (els.addForm) {
      els.addForm.addEventListener("submit", function (e) {
        e.preventDefault();

        var name = trim(els.addInput ? els.addInput.value : "");
        if (!name) {
          showToast("Введите имя сервиса, домен, IP или ASN", true);
          return;
        }

        handlePromise(withButton(els.addBtn, function () {
          return addService(name).then(function () {
            if (els.addInput) els.addInput.value = "";
          });
        }));
      });
    }

    if (els.body) {
      els.body.addEventListener("change", function (e) {
        var cb = e.target;
        if (!cb || !cb.classList || !cb.classList.contains("svc-row-check")) return;

        toggleSelect(cb.getAttribute("data-key"), cb.getAttribute("data-service"), cb.checked);
      });

      els.body.addEventListener("click", function (e) {
        var btn = closest(e.target, "button[data-action]");
        if (!btn) return;

        var name = btn.getAttribute("data-service");
        var action = btn.getAttribute("data-action");

        if (!name) return;

        if (action === "details") {
          openDrawer(name);
        } else if (action === "sync") {
          handlePromise(syncOne(name, false, btn));
        } else if (action === "dry") {
          handlePromise(syncOne(name, true, btn));
        } else if (action === "delete") {
          openDelete(name);
        }
      });
    }

    each(qsa("[data-svc-close]"), function (el) {
      el.addEventListener("click", closeDrawer);
    });

    each(qsa(".svc-tab"), function (tab) {
      tab.addEventListener("click", function () {
        state.details.tab = tab.getAttribute("data-svc-tab") || "overview";
        renderDetails();
      });
    });

    if (els.drawerSync) {
      els.drawerSync.addEventListener("click", function () {
        if (!state.details.name) return;
        handlePromise(syncOne(state.details.name, false, els.drawerSync));
      });
    }

    if (els.drawerDry) {
      els.drawerDry.addEventListener("click", function () {
        if (!state.details.name) return;
        handlePromise(syncOne(state.details.name, true, els.drawerDry));
      });
    }

    if (els.drawerDelete) {
      els.drawerDelete.addEventListener("click", function () {
        if (!state.details.name) return;
        openDelete(state.details.name);
      });
    }

    if (els.deleteCancel) {
      els.deleteCancel.addEventListener("click", closeDelete);
    }

    each(qsa("[data-svc-delete-cancel]"), function (el) {
      el.addEventListener("click", closeDelete);
    });

    if (els.deleteKeep) {
      els.deleteKeep.addEventListener("click", function () {
        if (!state.delete.name) return;
        handlePromise(deleteService(state.delete.name, false));
      });
    }

    if (els.deletePurge) {
      els.deletePurge.addEventListener("click", function () {
        if (!state.delete.name) return;

        var ok = window.confirm(
          "Удалить сервис " + state.delete.name + " и все маршруты AUTO:" + state.delete.name + "?"
        );

        if (!ok) return;

        handlePromise(deleteService(state.delete.name, true));
      });
    }

    document.addEventListener("keydown", function (e) {
      var tag = document.activeElement && document.activeElement.tagName;
      var typing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";

      if (e.key === "/" && !typing && els.search) {
        e.preventDefault();
        els.search.focus();
      }

      if ((e.ctrlKey || e.metaKey) && e.key === "r") {
        e.preventDefault();
        handlePromise(loadAll(true));
      }

      if (e.key === "Escape") {
        if (state.delete.open) {
          closeDelete();
        } else if (isDrawerOpen()) {
          closeDrawer();
        }
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
