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

  // Флаги MikroTik приходят строками "true"/"false" (а не булевыми):
  // только явное "true"/true считается включённым.
  function flag(value) {
    return value === true || lower(value) === "true";
  }

  function pluralRu(n, one, few, many) {
    var abs = Math.abs(n) % 100;
    var n1 = abs % 10;
    if (abs > 10 && abs < 20) return many;
    if (n1 > 1 && n1 < 5) return few;
    if (n1 === 1) return one;
    return many;
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

  // Единый стек уведомлений (реализация в app.js: window.mrsToast).
  function showToast(message, isError) {
    if (window.mrsToast) {
      window.mrsToast(message, isError);
    }
  }

  function fallbackCopy(textValue) {
    var textarea = document.createElement("textarea");
    textarea.value = textValue;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "fixed";
    textarea.style.top = "-1000px";
    textarea.style.left = "-1000px";
    textarea.style.opacity = "0";
    document.body.appendChild(textarea);
    textarea.select();
    try {
      document.execCommand("copy");
    } catch (e) {}
    document.body.removeChild(textarea);
  }

  function copyText(textValue) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(textValue).then(function () {
        showToast("Скопировано: " + textValue);
      }, function () {
        fallbackCopy(textValue);
        showToast("Скопировано: " + textValue);
      });
    } else {
      fallbackCopy(textValue);
      showToast("Скопировано: " + textValue);
    }
  }

  var els = {
    alert: byId("svc-alert"),
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
    addMethod: byId("svc-add-method"),
    addPreset: byId("svc-add-preset"),
    addBtn: byId("svc-add-btn"),
    addStatus: byId("svc-add-status"),
    body: byId("services-body"),
    empty: byId("services-empty"),
    drawer: byId("svc-drawer"),
    drawerTitle: byId("svc-drawer-title"),
    drawerSubtitle: byId("svc-drawer-subtitle"),
    drawerBody: byId("svc-drawer-body"),
    drawerSync: byId("svc-drawer-sync"),
    drawerDry: byId("svc-drawer-dry"),
    drawerDelete: byId("svc-drawer-delete"),
    drawerMenuBtn: byId("svc-drawer-menu"),
    drawerMenu: byId("svc-drawer-menu-list"),
    deleteModal: byId("svc-delete-modal"),
    deleteName: byId("svc-delete-name"),
    deleteKeep: byId("svc-delete-keep"),
    deletePurge: byId("svc-delete-purge"),
    deleteCancel: byId("svc-delete-cancel"),
    diffModal: byId("svc-diff-modal"),
    diffSubtitle: byId("svc-diff-subtitle"),
    diffSearch: byId("svc-diff-search"),
    diffOnly: byId("svc-diff-only"),
    diffList: byId("svc-diff-list"),
    diffClose: byId("svc-diff-close")
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
    progress: {},
    loading: false,
    booted: false,
    collapsed: {},
    lastDiff: {},
    lastResult: {},
    diff: null,
    details: {
      open: false,
      name: null,
      tab: "overview",
      loading: false,
      history: [],
      logs: [],
      routes: [],
      routesMeta: null,
      routesQuery: ""
    },
    cdn: null,
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
  }

  function setKpi(valueId, noteId, value, note, stateName) {
    var v = byId(valueId);
    var n = byId(noteId);

    if (v) {
      v.textContent = text(value);
      v.title = text(value);
    }
    if (n) n.textContent = text(note);

    var card = v && v.closest ? v.closest(".svc-kpi-card") : null;
    if (card) {
      card.className = "svc-kpi-card" + (stateName ? " svc-state-" + stateName : "");
    }
  }

  function parseTime(value) {
    if (value === null || value === undefined || value === "") return null;

    if (typeof value === "number") {
      var d = new Date(value > 1000000000000 ? value : value * 1000);
      return isNaN(d.getTime()) ? null : d;
    }

    var s = String(value).trim();
    var m = /^(\d{4})-(\d{2})-(\d{2})[Tt ](\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(Z|z|[+-]\d{2}:?\d{2})?$/.exec(s);
    if (!m) {
      var parsed = new Date(s);
      return isNaN(parsed.getTime()) ? null : parsed;
    }
    // Go отдаёт RFC3339 с наносекундами — собираем дату из компонент,
    // часть движков не парсит дробную часть длиннее миллисекунд (иначе NaN
    // ломает всю витрину: даты превращаются в «—»).
    var ms = parseInt(((m[7] || "0") + "000").slice(0, 3), 10);
    if (!m[8]) {
      return new Date(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6], ms);
    }
    var utc = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6], ms);
    var tz = m[8];
    if (tz !== "Z" && tz !== "z") {
      var digits = tz.slice(1).replace(":", "");
      var off = (+digits.slice(0, 2)) * 60 + (+digits.slice(2, 4));
      utc += (tz[0] === "-" ? 1 : -1) * off * 60000;
    }
    var t = new Date(utc);
    return isNaN(t.getTime()) ? null : t;
  }

  function formatClock(value) {
    var d = parseTime(value);
    if (!d) return "—";
    return pad(d.getDate()) + "." + pad(d.getMonth() + 1) + "." + d.getFullYear() + " " +
           pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
  }

  function formatDateTime(value) {
    var d = parseTime(value);
    if (!d) return "—";
    return pad(d.getDate()) + "." + pad(d.getMonth() + 1) + "." + d.getFullYear() + " " +
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

    return { key: "custom", label: "своё", className: "svc-badge-auto" };
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
          sunday: "воскресеньям",
          monday: "понедельникам",
          tuesday: "вторникам",
          wednesday: "средам",
          thursday: "четвергам",
          friday: "пятницам",
          saturday: "субботам"
        };
        return "по " + (days[w[1].toLowerCase()] || w[1]) + " в " + w[2] + ":" + w[3];
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

  // serviceGUID — идентификатор текущего воплощения сервиса (UUID v4).
  function serviceGUID(row) {
    if (!row) return "";
    return text(row.GUID || row.guid || row.ServiceID || row.service_id);
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
    var override = normalizeSpec(getScheduleOverrideSpec(name));
    if (override) return "service";

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

  // historyServiceID — id воплощения сервиса у записи истории (пусто у
  // записей прежних версий/до появления идентификаторов).
  function historyServiceID(rec) {
    if (!rec) return "";
    return text(rec.service_id || rec.ServiceID || rec.serviceId || rec.guid || rec.GUID);
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

  function statusNote(status) {
    var map = {
      success: "успех",
      error: "ошибка",
      warning: "предупреждение",
      running: "выполняется",
      unknown: "нет данных"
    };
    return map[status] || map.unknown;
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
      // Контракт бэкенда — поле removed (Result.Removed, history.Record.Removed).
      deleted: numField(rec, ["removed", "Removed", "deleted", "routes_deleted", "del", "Deleted"]),
      unchanged: numField(rec, ["unchanged", "same", "unchanged_count", "Unchanged"])
    };
  }

  // Сумма отсева валидатора (поле rejected результата синка).
  function rejectedTotal(r) {
    if (!r) return 0;
    return numField(r, ["ipv6", "IPv6"]) +
      numField(r, ["blocked", "Blocked"]) +
      numField(r, ["too_broad", "TooBroad"]) +
      numField(r, ["host_routes", "HostRoute"]) +
      numField(r, ["excluded", "Excluded"]) +
      numField(r, ["not_included", "NotIncluded"]);
  }

  // Краткие флаги качества для тоста после синка.
  function qualityFlags(d) {
    var out = [];
    if (!d) return out;
    if (d.coverage && d.coverage.checked > 0 && d.coverage.covered < d.coverage.checked) {
      out.push("покрытие " + d.coverage.covered + "/" + d.coverage.checked);
    }
    var vds = d.verification || [];
    for (var i = 0; i < vds.length; i++) {
      var vd = vds[i];
      if (vd && vd.checked > 0) {
        if (vd.failed && vd.failed.length) out.push("SNI-провал " + vd.domain);
        break; // шумит только primary-домен (первый проверяемый)
      }
    }
    var rej = rejectedTotal(d.rejected);
    if (rej > 0) out.push("отброшено " + rej);
    return out;
  }

  // Проблема качества из записи истории (cov_*/sni_primary_failed/
  // rejected_total из metadata). Пусто — проблем нет или данных нет.
  function qualityProblem(rec) {
    if (!rec) return "";
    var checked = numField(rec, ["cov_checked"]);
    var covered = numField(rec, ["cov_covered"]);
    if (checked > 0 && covered < checked) {
      return "покрытие " + covered + "/" + checked;
    }
    if (rec.sni_primary_failed) return "SNI-провал";
    var rej = numField(rec, ["rejected_total"]);
    if (rej > 0) return "отброшено " + rej;
    return "";
  }

  function historySubtitle(rec) {
    if (!rec) return "нет данных";

    var parts = [];
    if (isDryRun(rec)) parts.push("тестовый запуск");

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
      warning: ["Предупр.", "svc-badge-warn"],
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

  // logServiceID — id воплощения сервиса у лог-записи (пусто у записей,
  // созданных до появления идентификаторов).
  function logServiceID(entry) {
    if (!entry) return "";
    return text(entry.service_id || entry.ServiceID || entry.ServiceId);
  }

  function logError(entry) {
    return text(entry && (entry.err || entry.error || entry.Error));
  }

  // currentServiceGUID — id текущего воплощения сервиса по имени (из строк
  // таблицы сервисов). Пусто — идентификаторов нет (старые записи/логи).
  function currentServiceGUID(name) {
    var key = lower(name);
    if (!key) return "";
    for (var i = 0; i < state.services.length; i++) {
      var row = state.services[i];
      if (lower(serviceName(row)) === key) return serviceGUID(row);
    }
    return "";
  }

  // historyMatchesService — запись относится к текущему воплощению сервиса.
  // Если у сервиса есть id, берём только записи с этим id (записи прежнего
  // воплощения, в т.ч. без id, отсекаются). Если id нет (старые сервисы,
  // добавленные до появления идентификаторов) — не фильтруем.
  function historyMatchesService(rec, name) {
    var guid = currentServiceGUID(name || historyService(rec));
    if (!guid) return true;
    return historyServiceID(rec) === guid;
  }

  // logMatchesService — лог-запись относится к текущему воплощению сервиса.
  // Если у сервиса есть id, берём только записи с этим id (записи прежнего
  // воплощения, в т.ч. без id, отсекаются). Если id нет — не фильтруем.
  function logMatchesService(entry, name) {
    var guid = currentServiceGUID(name || logService(entry));
    if (!guid) return true;
    return logServiceID(entry) === guid;
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
      if (!name || state.historyByService[name]) return;
      // Записи чужого воплощения (id не совпадает с текущим) не берём:
      // после «удалил → добавил» история должна выглядеть пустой.
      if (!historyMatchesService(rec, name)) return;
      state.historyByService[name] = rec;
    });

    state.errorCountByService = {};
    state.warnCountByService = {};

    each(state.logs, function (entry) {
      var name = lower(logService(entry));
      if (!name) return;

      // Логи прежнего воплощения сервиса (id не совпадает с текущим) не
      // учитываем: иначе после «удалил → добавил» бейдж ошибки не гаснет.
      if (!logMatchesService(entry, name)) return;

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
      return { label: "сбой", cls: "svc-badge-warn", priority: 1 };
    }

    var lastStatus = last ? historyStatus(last) : "unknown";

    if (errorCount > 0 || lastStatus === "error") {
      return {
        label: errorCount > 1
          ? errorCount + " " + pluralRu(errorCount, "ошибка", "ошибки", "ошибок")
          : "ошибка",
        cls: "svc-badge-err",
        priority: 0
      };
    }

    if (warnCount > 0 || lastStatus === "warning") {
      return {
        label: warnCount > 1 ? warnCount + " предупр." : "предупреждение",
        cls: "svc-badge-warn",
        priority: 1
      };
    }

    // Качество последнего сбора из истории (покрытие, SNI primary, отсев):
    // успешный синк с дырами — предупреждение, а не OK. Проверка обязана
    // стоять до ветки success, иначе недостижима (статус у записи один).
    var qp = qualityProblem(last);
    if (qp) {
      return { label: qp, cls: "svc-badge-warn", priority: 1 };
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
      var ovMethod = text(row.Method || row.method);
      var ovDomains = row.Domains || row.domains || [];

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
        configOverride: configOverride,
        ovMethod: ovMethod,
        ovDomains: Array.isArray(ovDomains) ? ovDomains : []
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

    setKpi(
      "svc-total-value",
      "svc-total-note",
      String(total),
      withRoutes + " с записями",
      total ? "muted" : "muted"
    );

    setKpi(
      "svc-routes-value",
      "svc-routes-note",
      String(totalRoutes()),
      totalRoutes() ? "управляемых адресов" : "записей пока нет",
      totalRoutes() ? "ok" : "muted"
    );

    setKpi(
      "svc-auto-value",
      "svc-auto-note",
      String(auto),
      (total - auto) + " ручных/отключенных/нет",
      auto ? "ok" : "muted"
    );

    setKpi(
      "svc-problems-value",
      "svc-problems-note",
      String(problems),
      problems ? "ошибки, предупреждения, сбои" : "проблем не обнаружено",
      problems ? "err" : "ok"
    );
  }

  function rowHtml(item) {
    var checked = state.selection[item.key] ? " checked" : "";
    var selectedClass = state.selection[item.key] ? " svc-selected" : "";

    var nextHtml = item.next
      ? '<div class="svc-muted svc-small">' + esc(relativeNext(item.next)) + "</div>"
      : "";

    var lastHtml = item.last
      ? statusBadge(historyStatus(item.last)) + '<div class="svc-muted svc-small">' + esc(relativeTime(historyTime(item.last))) + "</div>"
      : '<span class="svc-muted">—</span>';

    // Бейдж группы — здесь; счётчики ошибок/предупреждений не дублируем:
    // они уже отражены в колонке «Состояние» через health.
    var badges = "";
    if (item.group) badges += '<span class="svc-badge svc-badge-group">' + esc(item.group) + "</span>";

    return '<tr class="' + selectedClass + '" data-key="' + esc(item.key) + '" data-service="' + esc(item.name) + '">' +
      '<td data-label="Выбор" class="mrs-corner"><input type="checkbox" class="svc-row-check" data-key="' + esc(item.key) + '" data-service="' + esc(item.name) + '"' + checked + "></td>" +
      '<td data-label="Сервис" class="mrs-title">' +
        '<div class="svc-name"><strong>' + esc(item.name) + "</strong>" + badges + "</div>" +
        '<div class="svc-progress" hidden>' +
          '<div class="svc-progress-track"><div class="svc-progress-fill"></div></div>' +
          '<div class="svc-progress-label"></div>' +
          '<div class="svc-progress-detail"></div>' +
        "</div>" +
      "</td>" +
      '<td data-label="Состояние"><span class="svc-badge ' + esc(item.health.cls) + '">' + esc(item.health.label) + "</span></td>" +
      '<td data-label="Расписание">' +
        "<div>" + esc(humanSchedule(item.effective)) + "</div>" + nextHtml +
      "</td>" +
      '<td data-label="Записей">' + esc(String(item.routes)) + "</td>" +
      '<td data-label="Последняя синхр.">' + lastHtml + "</td>" +
      '<td data-label="Действия" class="mrs-flat">' +
        '<div class="svc-actions">' +
          '<button type="button" class="svc-outline" data-action="details" data-service="' + esc(item.name) + '">Детали</button>' +
          '<button type="button" data-action="dry" title="Тестовый запуск" data-service="' + esc(item.name) + '">Тест</button>' +
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
      if (state.items.length) {
        els.empty.innerHTML = "Ничего не найдено. Измените поиск или фильтр. " +
          '<button type="button" class="btn btn-small btn-outline" data-svc-empty-reset>Сбросить поиск и фильтр</button>';
      } else {
        els.empty.innerHTML = "Нет сервисов. " +
          '<button type="button" class="btn btn-small btn-primary" data-svc-add-focus>Добавить первый сервис</button>';
      }
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
    updateProgressRows();
  }

  // ---------- Прогресс синхронизации (поллинг /api/v1/progress) ----------
  var progressPoll = { timer: null, empty: 0, sawActive: false };

  function startProgressPoll() {
    progressPoll.empty = 0;
    progressPoll.sawActive = false;
    if (progressPoll.timer) return;
    progressPoll.timer = setInterval(pollProgressOnce, 900);
    pollProgressOnce();
  }

  function stopProgressPoll() {
    if (progressPoll.timer) {
      clearInterval(progressPoll.timer);
      progressPoll.timer = null;
    }
    progressPoll.empty = 0;
    progressPoll.sawActive = false;
    state.progress = {};
    updateProgressRows();
  }

  function pollProgressOnce() {
    return api("/api/v1/progress").then(function (map) {
      var active = map && typeof map === "object" ? map : {};

      if (Object.keys(active).length) {
        progressPoll.empty = 0;
        progressPoll.sawActive = true;
        state.progress = active;
      } else {
        progressPoll.empty++;
        // Активность была — 2 пустых подряд, её уже нет: стоп.
        // Не было вовсе (POST не стартанул) — ~11 с и стоп.
        if ((progressPoll.sawActive && progressPoll.empty >= 2) ||
            (!progressPoll.sawActive && progressPoll.empty >= 12)) {
          stopProgressPoll();
          return;
        }
      }
      updateProgressRows();
    }).catch(function () {
      // Сеть моргнула — оставляем последнее известное состояние.
    });
  }

  function updateProgressRows() {
    if (!els.body) return;

    each(els.body.querySelectorAll("tr[data-service]"), function (tr) {
      var name = tr.getAttribute("data-service");
      var p = state.progress[name] || state.progress[lower(name)];
      var box = tr.querySelector(".svc-progress");
      if (!box) return;

      if (!p) {
        box.hidden = true;
        return;
      }

      var pct = Math.max(0, Math.min(100, Number(p.percent) || 0));
      box.hidden = false;

      var fill = box.querySelector(".svc-progress-fill");
      if (fill) fill.style.width = pct + "%";

      var label = box.querySelector(".svc-progress-label");
      if (label) {
        label.textContent = (p.dry_run ? "Тест: " : "") + text(p.phase) + " · " + pct + "%";
      }

      var detail = box.querySelector(".svc-progress-detail");
      if (detail) {
        var dtext = text(p.detail);
        detail.textContent = dtext;
        detail.hidden = !dtext;
      }
    });
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
  }

  function toggleSelect(key, name, checked) {
    if (checked) {
      state.selection[key] = name;
    } else {
      delete state.selection[key];
    }
    updateSelectionUI();
  }

  function withButton(btn, fn, busyLabel) {
    if (!btn) return Promise.resolve(fn());

    var old = btn.textContent;
    btn.disabled = true;
    btn.textContent = busyLabel || "…";

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

    // Первая загрузка: skeleton вместо пустой таблицы.
    if (!state.booted && els.body) {
      els.body.innerHTML = skeletonHtml();
      if (els.empty) els.empty.hidden = true;
    }

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
      state.booted = true;

      // Страница могла быть открыта в момент чужой синхронизации.
      pollProgressOnce();

      if (state.details.open) {
        renderDetails();
        if (state.details.tab === "routes") {
          loadRoutes();
        }
      }
    }).catch(function (err) {
      setAlert("error", "Ошибка загрузки страницы: " + text(err && err.message));
    }).then(function () {
      setRefreshing(false);
    });
  }

  function skeletonHtml() {
    var h = "";
    for (var i = 0; i < 4; i++) {
      h += '<tr class="svc-skel-row"><td colspan="7">' +
        '<div class="svc-skel w60"></div>' +
        '<div class="svc-skel w80"></div>' +
        "</td></tr>";
    }
    return h;
  }

  function addService(name, method, preset) {
    var body = { name: name };
    if (method) body.method = method;
    if (preset) body.preset = preset;

    // Пока идёт автоопределение CDN (может занять секунды), показываем
    // подшаги: POST висит, поэтому статус тянем отдельным эндпоинтом.
    // При явном методе детект на сервере не запускается — не поллим.
    var poll = method ? null : startAddStatusPoll(name);
    return api("/api/v1/services", {
      method: "POST",
      body: JSON.stringify(body)
    }).then(function () {
      showToast("Сервис добавлен: " + name);
      return loadAll(true);
    }).then(function (v) {
      stopAddStatusPoll(poll);
      return v;
    }, function (err) {
      stopAddStatusPoll(poll);
      throw err;
    });
  }

  // startAddStatusPoll опрашивает статус добавления, пока идёт POST.
  function startAddStatusPoll(name) {
    var poll = { timer: null, stopped: false };
    if (!els.addStatus) return poll;

    els.addStatus.hidden = false;
    els.addStatus.textContent = "проверка источника…";

    function tick() {
      if (poll.stopped) return;
      api("/api/v1/services/add-status?name=" + encodeURIComponent(name))
        .then(function (data) {
          if (poll.stopped) return;
          var detail = data && data.detail ? text(data.detail) : "";
          if (detail) els.addStatus.textContent = detail;
        })
        .catch(function () {
          // Статус — вспомогательный: сеть моргнула, продолжаем молча.
        });
    }

    tick();
    poll.timer = setInterval(tick, 500);
    return poll;
  }

  function stopAddStatusPoll(poll) {
    if (!poll) return;
    poll.stopped = true;
    if (poll.timer) {
      clearInterval(poll.timer);
      poll.timer = null;
    }
    if (els.addStatus) {
      els.addStatus.hidden = true;
      els.addStatus.textContent = "";
    }
  }

  // Автоподсказка пресета по вводимому имени (инвертированный индекс
  // полного зеркала v2fly). Тихо: нет совпадений — нет подсказки.
  function suggestPreset() {
    var hint = byId("svc-preset-hint");
    if (!hint || !els.addInput || !els.addPreset) return;
    var q = trim(els.addInput.value);
    if (q.length < 3 || (els.addPreset.value || "") !== "") {
      hint.textContent = "";
      return;
    }
    api("/api/v1/presets/match?domain=" + encodeURIComponent(q)).then(function (data) {
      var list = (data && data.matches) || [];
      if (!list.length || trim(els.addInput.value) !== q) return;
      // Применяем только вендоренный (его option есть в select);
      // невендоренные показываем текстом — их нельзя создать.
      var vendored = null;
      var nonVendoredFirst = null;
      var names = [];
      // Слаг сервера ограничен [a-z0-9_-]: "geolocation-!cn" создать нельзя,
      // кнопка даёт постоянный 400 — берём первый невендоренный валидный.
      var slugOk = /^[a-z0-9][a-z0-9_-]{0,63}$/;
      each(list, function (m) {
        names.push(m.title || m.slug);
        if (!vendored && m.vendored) vendored = m;
        if (!nonVendoredFirst && !m.vendored && slugOk.test(m.slug)) nonVendoredFirst = m;
      });
      hint.innerHTML = "";
      if (vendored) {
        var b = document.createElement("button");
        b.type = "button";
        b.className = "btn btn-small btn-outline";
        b.textContent = "Похоже: " + (vendored.title || vendored.slug) + " — применить";
        b.addEventListener("click", function () {
          var found = false;
          each(els.addPreset.options, function (o) {
            if (o.value === vendored.slug) found = true;
          });
          if (!found) {
            var opt = document.createElement("option");
            opt.value = vendored.slug;
            opt.textContent = vendored.title || vendored.slug;
            els.addPreset.appendChild(opt);
          }
          els.addPreset.value = vendored.slug;
          hint.textContent = "";
          els.addPreset.focus();
        });
        hint.appendChild(b);
        // Вторая кнопка: создать пресет из первого невендоренного списка.
        if (nonVendoredFirst) {
          var b2 = document.createElement("button");
          b2.type = "button";
          b2.className = "btn btn-small btn-outline";
          b2.style.marginLeft = "6px";
          b2.textContent = "Создать из списка «" + (nonVendoredFirst.title || nonVendoredFirst.slug) + "»";
          b2.addEventListener("click", function () {
            b2.disabled = true;
            b2.textContent = "Создание…";
            api("/api/v1/presets/create-from-upstream", {
              method: "POST",
              body: JSON.stringify({ slug: nonVendoredFirst.slug })
            }).then(function (resp) {
              var warn = resp.warning ? " ⚠ " + resp.warning : "";
              showToast("Пресет создан: " + resp.title + " (" + resp.domains + " доменов)" + warn);
              // Перезагружаем список пресетов и ставим только что созданный.
              state.presets = undefined;
              loadPresets().then(function () {
                var found = false;
                each(els.addPreset.options, function (o) {
                  if (o.value === resp.slug) found = true;
                });
                if (!found) {
                  var opt = document.createElement("option");
                  opt.value = resp.slug;
                  opt.textContent = resp.title || resp.slug;
                  els.addPreset.appendChild(opt);
                }
                els.addPreset.value = resp.slug;
                hint.textContent = "";
              });
            }).catch(function (e) {
              showToast("Ошибка: " + (e && e.message ? e.message : e));
            }).finally(function () {
              b2.disabled = false;
              b2.textContent = "Создать из списка «" + (nonVendoredFirst.title || nonVendoredFirst.slug) + "»";
            });
          });
          hint.appendChild(b2);
        }
      } else {
        hint.textContent = "Похоже: " + names.slice(0, 3).join(", ") + " (нет в каталоге)";
      }
    }).catch(function () { /* тихо */ });
  }
  // Large-пресеты без доменов в списке — создание по ним сервер отклонит
  // с объяснением (rublock), в option помечаем.
  function loadPresets() {
    if (state.presets !== undefined) return Promise.resolve();
    state.presets = null;
    return api("/api/v1/presets").then(function (data) {
      var list = (data && data.presets) || [];
      state.presets = list;
      if (!els.addPreset) return;
      var html = '<option value="">Пресет: нет</option>';
      each(list, function (p) {
        var label = text(p.title || p.slug);
        if (p.large) label += " (большой)";
        else if (p.domains && p.domains.length) label += " (" + p.domains.length + ")";
        html += '<option value="' + esc(p.slug) + '">' + esc(label) + "</option>";
      });
      els.addPreset.innerHTML = html;
    }).catch(function () {
      state.presets = null;
    });
  }

  // Снимок адресов сервиса для diff
  function fetchAddrs(name) {
    return api("/api/v1/address-list/" + encodeURIComponent(name)).then(function (d) {
      var list = (d && (d.entries || d.Entries)) || [];
      var set = {};
      each(list, function (e) {
        var a = text(e.Address || e.address);
        if (a) set[a] = true;
      });
      return set;
    }).catch(function () {
      return null;
    });
  }

  function syncOne(name, dry, btn) {
    var path = dry
      ? "/api/v1/services/" + encodeURIComponent(name) + "/dry-run"
      : "/api/v1/services/" + encodeURIComponent(name) + "/sync";

    var body = dry ? "{}" : JSON.stringify({ dry_run: false, force: false });

    return withButton(btn, function () {
      startProgressPoll();

      var beforeP = dry ? Promise.resolve(null) : fetchAddrs(name);

      return beforeP.then(function (before) {
        return api(path, {
          method: "POST",
          body: body
        }).then(function (data) {
          var c = historyCounts(data);
          var parts = [];

          if (c.added) parts.push("+" + c.added);
          if (c.deleted) parts.push("-" + c.deleted);
          if (c.unchanged) parts.push("=" + c.unchanged);

          // Полный результат (покрытие/SNI/отсев) — в drawer «Обзор»,
          // краткие флаги — в тост: раньше качество молча выбрасывалось.
          state.lastResult[lower(name)] = data || null;

          var msg = (dry ? "Тестовый запуск " : "Синхронизация ") + name;
          if (parts.length) msg += ": " + parts.join(" ");
          var flags = qualityFlags(data);
          if (flags.length) msg += " · ⚠ " + flags.join(", ");
          // Fail-open dry-run: роутер был недоступен, цифры условные.
          if (dry && data && data.mikrotik_down) {
            msg += " · ⚠ MikroTik недоступен, результат условный";
          }

          showToast(msg);

          // Diff «до/после» для реальной синхронизации
          if (!dry && before) {
            fetchAddrs(name).then(function (after) {
              if (!after) return;

              var added = [], removed = [], same = [];
              Object.keys(after).forEach(function (a) {
                if (before[a]) same.push(a); else added.push(a);
              });
              Object.keys(before).forEach(function (a) {
                if (!after[a]) removed.push(a);
              });

              var diff = { service: name, added: added, removed: removed, same: same };
              state.lastDiff[name] = diff;
              // Модалку изменений не открываем — она доступна из drawer
              // («Показать изменения»); после «Синхр.» хватает toast.
            });
          }

          setTimeout(function () {
            loadAll(true);
          }, 1000);
        });
      });
    });
  }

  function bulkAction(names, dry, btn) {
    if (!names.length) return Promise.resolve();

    return withButton(btn, function () {
      startProgressPoll();

      return api("/api/v1/services/sync", {
        method: "POST",
        body: JSON.stringify({
          services: names,
          dry_run: dry,
          force: false
        })
      }).then(function () {
        showToast((dry ? "Тестовый запуск запущен: " : "Синхронизация запущена: ") + names.length + " сервисов");
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
      showToast("Сервис удалён: " + name + (purge ? " вместе с записями" : ""));
      delete state.selection[lower(name)];
      closeDelete();
      closeDrawer();
      return loadAll(true);
    });
  }

  // ---------- Diff ----------
  function openDiff(diff) {
    state.diff = diff;
    if (!els.diffModal) return;

    if (els.diffSubtitle) {
      els.diffSubtitle.textContent =
        diff.service + " · +" + diff.added.length + " / −" + diff.removed.length +
        " / =" + diff.same.length + " · " + formatClock(new Date().toISOString());
    }

    els.diffModal.hidden = false;
    document.body.classList.add("svc-no-scroll");
    renderDiff();
  }

  function closeDiff() {
    if (els.diffModal) els.diffModal.hidden = true;
    if (!isDrawerOpen() && !state.delete.open) {
      document.body.classList.remove("svc-no-scroll");
    }
  }

  function renderDiff() {
    if (!els.diffList || !state.diff) return;

    var q = lower(els.diffSearch ? els.diffSearch.value : "");
    var only = els.diffOnly ? els.diffOnly.checked : true;

    var rows = [];
    each(state.diff.added, function (a) { rows.push({ t: "+", a: a }); });
    each(state.diff.removed, function (a) { rows.push({ t: "-", a: a }); });
    if (!only) {
      each(state.diff.same, function (a) { rows.push({ t: "=", a: a }); });
    }

    rows.sort(function (x, y) { return x.a.localeCompare(y.a); });

    if (q) {
      rows = rows.filter(function (r) {
        return r.a.toLowerCase().indexOf(q) !== -1;
      });
    }

    if (!rows.length) {
      els.diffList.innerHTML = '<div class="svc-empty">Нет префиксов по заданным условиям.</div>';
      return;
    }

    var html = "";
    each(rows, function (r) {
      var cls = r.t === "+" ? "svc-diff-add" : r.t === "-" ? "svc-diff-del" : "svc-diff-same";
      html += '<div class="svc-diff-row ' + cls + '">' + r.t + " " + esc(r.a) + "</div>";
    });
    els.diffList.innerHTML = html;
  }

  // ---------- Drawer ----------
  var drawerOpener = null;

  function openDrawer(name) {
    if (document.activeElement && document.activeElement !== document.body) {
      drawerOpener = document.activeElement;
    } else {
      drawerOpener = null;
    }
    state.details.open = true;
    state.details.name = name;
    state.details.tab = "overview";
    state.details.loading = true;
    state.details.history = [];
    state.details.logs = [];
    state.details.routes = [];
    state.details.routesMeta = null;
    state.details.routesQuery = "";
    state.cdn = null;

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

    if (drawerOpener && drawerOpener.focus && document.contains(drawerOpener)) {
      drawerOpener.focus();
    }
    drawerOpener = null;

    if (els.drawer) {
      els.drawer.hidden = true;
      els.drawer.setAttribute("aria-hidden", "true");
    }

    if (!state.delete.open && !(els.diffModal && !els.diffModal.hidden)) {
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

    if (!isDrawerOpen() && !(els.diffModal && !els.diffModal.hidden)) {
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
      item.errorCount
        ? item.errorCount + " " + pluralRu(item.errorCount, "ошибка", "ошибки", "ошибок") + " в логах"
        : item.warnCount
          ? item.warnCount + " " + pluralRu(item.warnCount, "предупреждение", "предупреждения", "предупреждений") + " в логах"
          : "по истории и логам",
      item.health.priority === 0 ? "err" : item.health.priority === 1 ? "warn" : item.health.priority === 2 ? "ok" : "muted"
    );

    html += detailItem(
      "Записи",
      esc(String(item.routes)),
      item.routes ? "управляемые адреса" : "нет активных записей",
      item.routes ? "ok" : "muted"
    );

    html += detailItem(
      "Расписание",
      esc(humanSchedule(item.effective)),
      "",
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
      "Переопределение",
      item.configOverride ? '<span class="svc-badge svc-badge-override">есть</span>' : '<span class="svc-badge svc-badge-muted">нет</span>',
      item.scheduleOverride ? "есть переопределение расписания" : "наследуется из группы/глобально",
      item.configOverride ? "warn" : "muted"
    );

    html += detailItem(
      "Группа",
      item.group ? '<span class="svc-badge svc-badge-group">' + esc(item.group) + "</span>" : "—",
      item.group ? "сервис входит в группу расписаний" : "не входит в группу",
      "muted"
    );

    html += "</div>";

    html += detailsHtml("quality", "Качество последнего сбора", lastQualityHtml(item));
    html += detailsHtml("source", "Источник сбора", sourceEditHtml(item));
    html += detailsHtml("cdn", "Подбор источника (CDN)", cdnBlockHtml());
    html += detailsHtml("domains", "Домены и пресеты", domainsBlockHtml(item));

    return html;
  }

  // Свочиваемая секция обзора. На телефоне стартует свёрнутой (длинный
  // скролл drawer), выбор пользователя запоминается в state.collapsed
  // и переживает перерендеры.
  function detailsHtml(key, title, inner) {
    var open;
    if (state.collapsed[key] !== undefined) {
      open = !state.collapsed[key];
    } else {
      open = !isMobileView();
    }
    return '<details class="svc-collapsible" data-collapse="' + esc(key) + '"' + (open ? " open" : "") + ">" +
      '<summary class="svc-block-title">' + esc(title) + "</summary>" + inner + "</details>";
  }

  function isMobileView() {
    return Boolean(window.matchMedia && window.matchMedia("(max-width: 768px)").matches);
  }

  // Редактор источника сбора (scope: method + domains). Остальные поля
  // override — через конфиг. Ошибки валидации показывает сервер (400).
  function sourceEditHtml(item) {
    var curMethod = item.ovMethod || "";
    var curDomains = (item.ovDomains || []).join(", ");
    var h = "<p>" + (curMethod
      ? "Метод: <b>" + esc(curMethod) + "</b> (override)"
      : "Метод: <b>авто</b> (классификатор)") + "</p>";
    h += '<label class="svc-field"><span>Метод</span>' +
      '<input id="svc-ov-method" value="' + esc(curMethod) + '" placeholder="dynamic,whois" spellcheck="false"></label>';
    h += '<label class="svc-field"><span>Домены (через запятую)</span>' +
      '<input id="svc-ov-domains" value="' + esc(curDomains) + '" placeholder="example.com, cdn.example.com" spellcheck="false"></label>';
    h += '<div class="svc-row"><button class="btn btn-small btn-primary" data-ov-save="1">Сохранить</button></div>';
    h += '<p class="svc-muted svc-small">Методы: cdn, asn, dynamic, whois, static_url; списки — через запятую. ' +
      "Пустой метод — авто. Тестовый прогон — кнопкой ниже.</p>";
    return h;
  }

  function saveOverride(name) {
    var item = findItem(name);
    var methodEl = byId("svc-ov-method");
    var domainsEl = byId("svc-ov-domains");
    if (!item || !methodEl || !domainsEl) return;

    var body = {};
    var m = trim(methodEl.value);
    if (m !== (item.ovMethod || "")) body.method = m;
    var d = [];
    each(trim(domainsEl.value).split(","), function (s) {
      s = trim(s);
      if (s) d.push(s);
    });
    if (d.join(",") !== (item.ovDomains || []).join(",")) body.domains = d;

    if (!("method" in body) && !("domains" in body)) {
      showToast("Без изменений — сохранять нечего");
      return;
    }

    handlePromise(api("/api/v1/services/" + encodeURIComponent(name), {
      method: "PUT",
      body: JSON.stringify(body)
    }).then(function () {
      showToast("Источник обновлён: " + name);
      return loadAll(true);
    }));
  }

  // CDN-блок во вкладке «Обзор»: подбор источника (проверка за Cloudflare
  // и другими CDN с официальными списками).
  function cdnBlockHtml() {
    var c = state.cdn;
    if (!c || c.name !== state.details.name) {
      return '<button class="btn btn-small btn-outline" data-cdn-run="1">Подобрать источник (CDN)</button>';
    }
    if (c.loading) {
      return '<div class="svc-cdn-loading">Проверка CDN…</div>';
    }
    if (c.error) {
      return (
        '<div class="svc-cdn-result"><p>Ошибка: ' + esc(c.error) + "</p>" +
        '<button class="btn btn-small btn-outline" data-cdn-run="1">Повторить</button></div>'
      );
    }

    var a = c.advice;
    if (!a) {
      return '<button class="btn btn-small btn-outline" data-cdn-run="1">Подобрать источник (CDN)</button>';
    }

    var h = '<div class="svc-cdn-result">';
    if (c.applied) {
      h +=
        '<span class="svc-badge svc-badge-ok">Метод изменён на cdn' +
        (c.purged ? ", удалено записей: " + c.purged : "") +
        "</span>";
    } else if (a.detected) {
      h +=
        "<p>" +
        esc(a.cdn) +
        " (AS" +
        a.asn +
        "): все адреса входят в официальный список (" +
        a.prefix_count +
        (a.whois_count ? " против whois — " + a.whois_count : "") +
        ")</p>";
      if (!a.recommend && a.reason) {
        h += "<p>" + esc(a.reason) + "</p>";
      }
      if (a.recommend) {
        h += '<button class="btn btn-small btn-primary" data-cdn-apply="1">Применить: метод cdn</button>';
      }
    } else {
      h += "<p>CDN не обнаружен: " + esc(a.reason || "") + "</p>";
      h += "<p>Рекомендация: оставить текущий метод (" + esc(a.current_method || "") + ")</p>";
    }
    h += qualityHtml(a);
    h += '<button class="btn btn-small btn-outline" data-cdn-run="1">Проверить снова</button>';
    h += "</div>";
    return h;
  }

  // Блок качества текущего сбора: покрытие A-записей, широкие префиксы,
  // SNI-проба, отсев валидации. Общий для advice (recommend) и Result (sync):
  // у advice поля rejected нет — блок просто не показывается.
  function qualityHtml(a) {
    var h = "";
    if (!a) return h;
    if (a.skipped && a.skipped.length) {
      h += "<p>Пропущены (не разрешаются): " + esc(a.skipped.join(", ")) + "</p>";
    }
    if (a.coverage && a.coverage.checked > 0) {
      h +=
        "<p>Покрытие: " +
        a.coverage.covered +
        "/" +
        a.coverage.checked +
        " A-записей входят в текущий набор";
      if (a.coverage.uncovered && a.coverage.uncovered.length) {
        h += " — вне набора: " + esc(a.coverage.uncovered.join(", "));
      }
      h += "</p>";
    }
    if (a.broad && a.broad.length) {
      h += "<p>Широкие префиксы: " + esc(a.broad.join(", ")) + "</p>";
    }
    if (a.verification && a.verification.length) {
      var primarySeen = false;
      for (var i = 0; i < a.verification.length; i++) {
        var vd = a.verification[i];
        if (!vd || !vd.checked) continue;
        // Первый проверяемый домен — основной (домен сервиса); остальные —
        // aux-инфраструктура, их провалы обычно ложные и не будят алерты.
        var tag = primarySeen ? "" : " (основной)";
        primarySeen = true;
        if (vd.failed && vd.failed.length) {
          h += "<p>SNI " + esc(vd.domain) + tag + ": ПРОВАЛ (" + esc(vd.failed.join(", ")) + ")</p>";
        } else {
          h += "<p>SNI " + esc(vd.domain) + tag + ": ok (" + vd.ok + "/" + vd.checked + ")</p>";
        }
      }
    }
    h += rejectedHtml(a.rejected);
    return h;
  }

  // Отсев валидации из результата синка: счётчики по причинам + хинт
  // про allow_host_routes (типичная причина пустого dynamic-сбора).
  function rejectedHtml(r) {
    var total = rejectedTotal(r);
    if (!r || total <= 0) return "";
    var parts = [];
    var pairs = [
      ["ipv6", "IPv6"], ["blocked", "заблокированные"], ["too_broad", "широкие"],
      ["host_routes", "хосты /32"], ["excluded", "исключены"], ["not_included", "вне include"]
    ];
    for (var i = 0; i < pairs.length; i++) {
      var n = numField(r, [pairs[i][0]]);
      if (n > 0) parts.push(pairs[i][1] + ": " + n);
    }
    var h = "<p>Отброшено валидацией: " + total + " (" + esc(parts.join(", ")) + ")";
    if (numField(r, ["host_routes"]) > 0) {
      h += " — включите safety.allow_host_routes (dynamic отдаёт только /32)";
    }
    h += "</p>";
    return h;
  }

  // Сводка качества из записи истории (персист; менее детально, чем
  // полный Result — только флаги).
  function historyQualityHtml(rec) {
    var h = "";
    if (!rec) return h;
    var checked = numField(rec, ["cov_checked"]);
    var covered = numField(rec, ["cov_covered"]);
    if (checked > 0) {
      h += "<p>Покрытие: " + covered + "/" + checked + " A-записей входят в набор";
      if (covered < checked) h += " — сервис частично не работает через VPN";
      h += "</p>";
    }
    if (rec.sni_primary_failed) {
      h += "<p>SNI основного домена: ПРОВАЛ — адреса не обслуживают домен</p>";
    }
    var rej = numField(rec, ["rejected_total"]);
    if (rej > 0) {
      h += "<p>Отброшено валидацией: " + rej + "</p>";
    }
    return h;
  }

  // Секция «Качество последнего сбора»: свежий результат синка →
  // advice проверки CDN → сводка из истории → заглушка.
  function lastQualityHtml(item) {
    var key = lower(item.name);
    if (state.lastResult[key]) {
      return '<p class="svc-muted">По последней синхронизации:</p>' + qualityHtml(state.lastResult[key]);
    }
    var c = state.cdn;
    if (c && c.name === item.name && c.advice) {
      return '<p class="svc-muted">По проверке CDN:</p>' + qualityHtml(c.advice);
    }
    var hq = historyQualityHtml(item.last);
    if (hq) {
      return '<p class="svc-muted">По истории (кратко):</p>' + hq;
    }
    return '<p class="svc-muted">Нет данных — запустите синхронизацию или проверку CDN.</p>';
  }

  function runCdnProbe(apply) {
    var name = state.details.name;
    if (!name) return;

    state.cdn = { name: name, loading: true };
    renderDetails();

    api("/api/v1/services/" + encodeURIComponent(name) + "/recommend", {
      method: "POST",
      body: JSON.stringify({ apply: Boolean(apply) })
    }).then(
      function (data) {
        if (state.details.name !== name) return;
        state.cdn = {
          name: name,
          advice: data && data.advice ? data.advice : null,
          applied: Boolean(data && data.applied),
          purged: data && typeof data.purged === "number" ? data.purged : 0
        };
        renderDetails();
        if (data && data.applied) {
          showToast(
            "Метод изменён на cdn" +
              (state.cdn.purged ? ", удалено записей: " + state.cdn.purged : "")
          );
          loadAll(false);
        }
      },
      function (err) {
        if (state.details.name !== name) return;
        var msg = String((err && err.message) || err);
        state.cdn = { name: name, error: msg };
        renderDetails();
        showToast("Не удалось проверить CDN: " + msg, true);
      }
    );
  }

  // Блок «Домены и пресеты» во вкладке «Обзор»: текущие домены, привязка,
  // подбор через urlscan (совет, не синк), обновление из пресета.
  function domainsBlockHtml(item) {
    var row = item.row || {};
    var domains = item.ovDomains || [];
    var h = '<div class="svc-domains">';
    h += "<p>" + (domains.length
      ? esc(domains.slice(0, 12).join(", ")) + (domains.length > 12 ? " …(+" + (domains.length - 12) + ")" : "")
      : "домены не заданы (автоопределение)") + "</p>";
    h += "<p>" + (row.Preset
      ? 'Пресет: <span class="svc-badge svc-badge-group">' + esc(row.Preset) + "</span>"
      : "Пресет: —") + "</p>";

    var sg = state.suggest;
    if (!sg || sg.name !== state.details.name) {
      h += '<button class="btn btn-small btn-outline" data-suggest-run="1">Подобрать домены (urlscan)</button>';
    } else if (sg.loading) {
      h += '<div class="svc-cdn-loading">Ищем смежные домены…</div>';
    } else if (sg.error) {
      h += '<div class="svc-cdn-result"><p>Ошибка: ' + esc(sg.error) + "</p>" +
        '<button class="btn btn-small btn-outline" data-suggest-run="1">Повторить</button></div>';
    } else if (sg.data && !sg.data.enabled) {
      h += '<div class="svc-cdn-result"><p>Подбор недоступен: ' + esc(sg.data.reason || "") + "</p></div>";
    } else if (sg.data && sg.data.candidates) {
      var cands = sg.data.candidates;
      if (!cands.length) {
        h += "<p>Кандидатов нет — попробуйте пресет или ручной ввод.</p>";
      } else {
        h += '<div class="svc-suggest-list">';
        each(cands, function (c, i) {
          var checked = c.confidence === "high" ? " checked" : "";
          h += '<label class="svc-suggest-row"><input type="checkbox" data-suggest-check="' + i + '"' + checked + "> " +
            "<code>" + esc(c.domain) + "</code> " +
            '<span class="svc-muted">' + esc(c.confidence || "") + ", в " + esc(String(c.scans || 0)) + " ск.</span></label>";
        });
        h += "</div>";
        h += '<button class="btn btn-small btn-primary" data-suggest-apply="1">Применить выбранные</button>';
      }
      var excl = sg.data.excluded || [];
      if (excl.length) {
        h += '<p class="svc-muted">Отсеяно: ' + esc(excl.slice(0, 8).map(function (e) { return e.domain; }).join(", ")) + "</p>";
      }
    }

    if (row.Preset) {
      h += '<div class="svc-preset-refresh"><button class="btn btn-small btn-outline" data-preset-refresh="1">Обновить домены из пресета</button></div>';
    }
    if (domains.length) {
      h += '<div class="svc-preset-refresh"><button class="btn btn-small btn-outline" data-blockcheck-run="1">Проверить блокировку (RU)</button>';
      var bc = state.blockcheck;
      if (bc && bc.name === state.details.name) {
        if (bc.loading) {
          h += ' <span class="svc-muted">Проверяем по блоклисту…</span>';
        } else if (bc.error) {
          h += ' <span class="svc-muted">Ошибка: ' + esc(bc.error) + "</span>";
        } else if (bc.data) {
          h += ' <span>' + esc(String(bc.data.blocked_count)) + " из " + esc(String(bc.data.total)) + " в блоклисте" +
            (bc.data.cached ? " (кэш)" : "") + "</span>";
          if ((bc.data.blocked || []).length) {
            h += '<div class="svc-muted">' + esc(bc.data.blocked.slice(0, 12).join(", ")) +
              (bc.data.blocked.length > 12 ? " …(+" + (bc.data.blocked.length - 12) + ")" : "") + "</div>";
          }
        }
      }
      h += "</div>";
    }
    var pr = state.presetRefresh;
    if (pr && pr.name === state.details.name) {
      if (pr.error) {
        h += '<div class="svc-cdn-result"><p>Ошибка: ' + esc(pr.error) + "</p></div>";
      } else if (pr.diff) {
        h += '<div class="svc-cdn-result"><p>+' + pr.diff.added.length + " / −" + pr.diff.removed.length +
          " — маршруты не тронуты, запустите тестовый прогон.</p></div>";
      }
    }
    h += "</div>";
    return h;
  }

  function runSuggest() {
    var name = state.details.name;
    if (!name) return;
    state.suggest = { name: name, loading: true };
    renderDetails();
    api("/api/v1/services/" + encodeURIComponent(name) + "/suggest-domains", {
      method: "POST",
      body: JSON.stringify({ scans: 5 })
    }).then(function (data) {
      if (state.details.name !== name) return;
      state.suggest = { name: name, data: data || null };
      renderDetails();
    }, function (err) {
      if (state.details.name !== name) return;
      var msg = String((err && err.message) || err);
      state.suggest = { name: name, error: msg };
      renderDetails();
      showToast("Не удалось подобрать домены: " + msg, true);
    });
  }

  function applySuggest() {
    var name = state.details.name;
    var sg = state.suggest;
    if (!name || !sg || !sg.data || !sg.data.candidates) return;
    var checked = [];
    each(els.drawerBody.querySelectorAll("[data-suggest-check]:checked"), function (cb) {
      var c = sg.data.candidates[Number(cb.getAttribute("data-suggest-check"))];
      if (c && c.domain) checked.push(c.domain);
    });
    if (!checked.length) {
      showToast("Ничего не выбрано");
      return;
    }
    var item = findItem(name);
    var merged = ((item && item.ovDomains) || []).slice();
    var seen = {};
    each(merged, function (d) { seen[lower(d)] = true; });
    each(checked, function (d) {
      if (!seen[lower(d)]) { merged.push(d); seen[lower(d)] = true; }
    });
    handlePromise(api("/api/v1/services/" + encodeURIComponent(name), {
      method: "PUT",
      body: JSON.stringify({ domains: merged })
    }).then(function () {
      showToast("Домены добавлены: +" + checked.length);
      return loadAll(true);
    }));
  }

  function refreshPreset(force) {
    var name = state.details.name;
    if (!name) return;
    state.presetRefresh = { name: name, loading: true };
    renderDetails();
    api("/api/v1/services/" + encodeURIComponent(name) + "/refresh-preset", {
      method: "POST",
      body: JSON.stringify({ force: Boolean(force) })
    }).then(function (data) {
      if (state.details.name !== name) return;
      state.presetRefresh = { name: name, diff: { added: (data && data.added) || [], removed: (data && data.removed) || [] } };
      renderDetails();
      showToast("Пресет применён: +" + state.presetRefresh.diff.added.length +
        " / −" + state.presetRefresh.diff.removed.length + ". Запустите тестовый прогон.");
      loadAll(true);
    }, function (err) {
      if (state.details.name !== name) return;
      var msg = String((err && err.message) || err);
      state.presetRefresh = { name: name, error: msg };
      renderDetails();
      showToast("Не удалось обновить пресет: " + msg, true);
    });
  }

  function runBlockcheck() {
    var name = state.details.name;
    if (!name) return;
    state.blockcheck = { name: name, loading: true };
    renderDetails();
    api("/api/v1/services/" + encodeURIComponent(name) + "/blockcheck", {
      method: "POST",
      body: "{}"
    }).then(function (data) {
      if (state.details.name !== name) return;
      state.blockcheck = { name: name, data: data || null };
      renderDetails();
    }, function (err) {
      if (state.details.name !== name) return;
      var msg = String((err && err.message) || err);
      state.blockcheck = { name: name, error: msg };
      renderDetails();
      showToast("Не удалось проверить блокировку: " + msg, true);
    });
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

  function routeAddr(e) {
    return text(e.Address || e.address);
  }

  function renderRoutesList(list) {
    var q = lower(state.details.routesQuery || "");

    var entries = (list || []).filter(function (e) { return routeAddr(e); });
    if (q) {
      entries = entries.filter(function (e) {
        return routeAddr(e).toLowerCase().indexOf(q) !== -1;
      });
    }

    // Область видимости: только записи ЭТОГО сервиса (list + AUTO:комментарий).
    var meta = state.details.routesMeta;
    var down = Boolean(meta && meta.down);
    var total = meta && typeof meta.count === "number" ? meta.count : (list || []).length;
    var scope = meta && (meta.list || meta.comment)
      ? (meta.list || "—") + " · " + (meta.comment || "—")
      : "";
    var summary = down
      ? ""
      : q
        ? "найдено " + entries.length + " из " + total + " " + pluralRu(total, "запись", "записи", "записей")
        : total + " " + pluralRu(total, "запись", "записи", "записей");

    var head =
      '<div class="svc-routes-meta">' + esc(scope && summary ? scope + " · " + summary : scope || summary) + "</div>" +
      '<div class="svc-routes-toolbar">' +
      '<div class="svc-routes-search">' +
      '<input type="search" id="svc-routes-search" placeholder="Поиск префикса" value="' +
      esc(state.details.routesQuery || "") + '">' +
      "</div>" +
      '<button type="button" class="btn btn-outline btn-small" data-svc-diff>Показать изменения</button>' +
      "</div>";

    if (down) {
      return head + '<div class="svc-empty">MikroTik недоступен: записи получить не удалось.</div>';
    }

    if (!entries.length) {
      return head + '<div class="svc-empty">Управляемых записей нет.</div>';
    }

    var rows = "";
    each(entries, function (e) {
      var a = routeAddr(e);
      var badges = "";
      // RouterOS отдаёт строки "true"/"false": непустая "false" — truthy,
      // поэтому сравниваем явно, иначе бейджи светятся на каждой записи.
      if (flag(e.Dynamic) || flag(e.dynamic)) badges += '<span class="svc-badge svc-badge-muted">динамическая</span>';
      if (flag(e.Disabled) || flag(e.disabled)) badges += '<span class="svc-badge svc-badge-disabled">отключено</span>';

      rows += '<div class="svc-route-row">' +
        "<span>" + esc(a) + " " + badges + "</span>" +
        '<button type="button" class="svc-route-copy svc-secondary" data-addr="' + esc(a) + '" title="Копировать">⧉</button>' +
        "</div>";
    });

    return head + "<div>" + rows + "</div>";
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
    } else if (state.details.tab === "routes") {
      els.drawerBody.innerHTML = renderRoutesList(state.details.routes);
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

      var hist = results[0].status === "fulfilled" && Array.isArray(results[0].value)
        ? results[0].value
        : [];

      // Отсекаем записи прежнего воплощения сервиса с тем же именем.
      var guid = currentServiceGUID(name);
      state.details.history = hist.filter(function (rec) {
        return historyMatchesService(rec, guid);
      });

      state.details.logs = results[1].status === "fulfilled" && Array.isArray(results[1].value)
        ? results[1].value.filter(function (e) {
            return logMatchesService(e, name);
          })
        : [];

      state.details.loading = false;
      renderDetails();
    });
  }

  // Записи ТОЛЬКО этого сервиса (API фильтрует по AUTO:<service>); загружаются
  // лениво при открытии вкладки «Записи» и обновляются после синхронизации.
  function loadRoutes() {
    var name = state.details.name;
    if (!name) return Promise.resolve();

    function done(rd) {
      if (state.details.name !== name) return;

      state.details.routes = (rd && (rd.entries || rd.Entries)) || [];
      state.details.routesMeta = rd
        ? {
            list: text(rd.list),
            comment: text(rd.comment),
            count: typeof rd.count === "number" ? rd.count : state.details.routes.length,
            down: Boolean(rd.mikrotik_down)
          }
        : null;

      if (!state.details.loading && state.details.tab === "routes" && els.drawerBody) {
        els.drawerBody.innerHTML = renderRoutesList(state.details.routes);
      }
    }

    return api("/api/v1/address-list/" + encodeURIComponent(name)).then(done, function () {
      if (state.details.name !== name) return;
      state.details.routes = [];
      state.details.routesMeta = { list: "", comment: "", count: 0, down: true };
      if (!state.details.loading && state.details.tab === "routes" && els.drawerBody) {
        els.drawerBody.innerHTML = renderRoutesList(state.details.routes);
      }
    });
  }

  function bindEvents() {
    // Кнопки в пустом состоянии таблицы (делегирование: контент пересоздаётся).
    if (els.empty) {
      els.empty.addEventListener("click", function (e) {
        var t = e.target;
        if (t && t.getAttribute && t.getAttribute("data-svc-add-focus") !== null) {
          if (els.addInput) {
            els.addInput.focus();
            if (els.addInput.scrollIntoView) {
              els.addInput.scrollIntoView({ block: "nearest" });
            }
          }
        }
        if (t && t.getAttribute && t.getAttribute("data-svc-empty-reset") !== null) {
          if (els.search) els.search.value = "";
          if (els.filter) els.filter.value = "all";
          renderTable();
          if (els.search) els.search.focus();
        }
      });
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

    if (els.addInput) {
      var presetHintTimer = null;
      els.addInput.addEventListener("input", function () {
        if (presetHintTimer) clearTimeout(presetHintTimer);
        presetHintTimer = setTimeout(suggestPreset, 400);
      });
    }

    if (els.addForm) {
      els.addForm.addEventListener("submit", function (e) {        e.preventDefault();

        var name = trim(els.addInput ? els.addInput.value : "");
        if (!name) {
          showToast("Введите имя сервиса, домен, IP или ASN", true);
          return;
        }

        var method = els.addMethod ? trim(els.addMethod.value) : "";
        var preset = els.addPreset ? trim(els.addPreset.value) : "";
        handlePromise(withButton(els.addBtn, function () {
          return addService(name, method, preset).then(function () {
            if (els.addInput) els.addInput.value = "";
            if (els.addMethod) els.addMethod.value = "";
            if (els.addPreset) els.addPreset.value = "";
          });
        }, method ? "" : "Проверяю источник…"));
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
        if (!btn) {
          // Клик по строке (мимо кнопок/чекбокса) открывает drawer:
          // на телефоне это основная мишень, кнопка «Детали» мелкая.
          if (closest(e.target, "input, select, textarea, a")) return;
          var tr = closest(e.target, 'tr[data-service]');
          if (tr && tr.getAttribute("data-service")) {
            openDrawer(tr.getAttribute("data-service"));
          }
          return;
        }

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
        if (state.details.tab === "routes") {
          loadRoutes();
        }
      });
    });

    // Поиск и копирование во вкладке «Записи» (делегирование)
    if (els.drawerBody) {
      els.drawerBody.addEventListener("input", function (e) {
        if (!e.target || e.target.id !== "svc-routes-search") return;

        state.details.routesQuery = e.target.value;
        els.drawerBody.innerHTML = renderRoutesList(state.details.routes);

        var inp = byId("svc-routes-search");
        if (inp) {
          inp.focus();
          inp.setSelectionRange(inp.value.length, inp.value.length);
        }
      });

      els.drawerBody.addEventListener("click", function (e) {
        var b = closest(e.target, ".svc-route-copy");
        if (b) {
          copyText(b.getAttribute("data-addr") || "");
          return;
        }
        if (closest(e.target, "[data-cdn-apply]")) {
          runCdnProbe(true);
          return;
        }
        if (closest(e.target, "[data-cdn-run]")) {
          runCdnProbe(false);
          return;
        }
        if (closest(e.target, "[data-suggest-run]")) {
          runSuggest();
          return;
        }
        if (closest(e.target, "[data-suggest-apply]")) {
          applySuggest();
          return;
        }
        if (closest(e.target, "[data-preset-refresh]")) {
          refreshPreset(false);
          return;
        }
        if (closest(e.target, "[data-blockcheck-run]")) {
          runBlockcheck();
          return;
        }
        if (closest(e.target, "[data-ov-save]")) {
          saveOverride(state.details.name);
        }
        // Кнопка diff живёт на вкладке «Записи» (футера drawer больше нет).
        if (closest(e.target, "[data-svc-diff]")) {
          var name = state.details.name;
          var d = name && state.lastDiff[name];
          if (d) {
            openDiff(d);
          } else {
            showToast("Изменения появятся после синхронизации, запущенной из интерфейса");
          }
        }
      });
      // toggle не всплывает — ловим на capture; выбор переживает перерендеры.
      els.drawerBody.addEventListener("toggle", function (e) {
        var d = e.target && e.target.closest ? e.target.closest("[data-collapse]") : null;
        if (!d) return;
        state.collapsed[d.getAttribute("data-collapse")] = !d.open;
      }, true);
    }

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

    // Меню ⋯ в шапке drawer: открыть/закрыть, мимо и Esc закрывают.
    function closeDrawerMenu() {
      if (els.drawerMenu) els.drawerMenu.hidden = true;
      if (els.drawerMenuBtn) els.drawerMenuBtn.setAttribute("aria-expanded", "false");
    }

    if (els.drawerMenuBtn && els.drawerMenu) {
      els.drawerMenuBtn.addEventListener("click", function (e) {
        if (e && e.stopPropagation) e.stopPropagation();
        var open = els.drawerMenu.hidden;
        if (open) {
          els.drawerMenu.hidden = false;
          els.drawerMenuBtn.setAttribute("aria-expanded", "true");
        } else {
          closeDrawerMenu();
        }
      });
      els.drawerMenu.addEventListener("click", function () {
        closeDrawerMenu();
      });
      document.addEventListener("click", function (e) {
        if (!els.drawerMenu.hidden && !(e.target && e.target.closest && e.target.closest(".svc-menu-wrap"))) {
          closeDrawerMenu();
        }
      });
      document.addEventListener("keydown", function (e) {
        if (e.key === "Escape") closeDrawerMenu();
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
          "Удалить сервис " + state.delete.name + " и все записи AUTO:" + state.delete.name + "?"
        );

        if (!ok) return;

        handlePromise(deleteService(state.delete.name, true));
      });
    }

    // Diff-модалка
    if (els.diffClose) els.diffClose.addEventListener("click", closeDiff);
    each(qsa("[data-diff-close]"), function (el) {
      el.addEventListener("click", closeDiff);
    });
    if (els.diffSearch) els.diffSearch.addEventListener("input", renderDiff);
    if (els.diffOnly) els.diffOnly.addEventListener("change", renderDiff);

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
        } else if (els.diffModal && !els.diffModal.hidden) {
          closeDiff();
        } else if (isDrawerOpen()) {
          closeDrawer();
        }
      }

      // Ловушка фокуса в drawer: Tab циклируется внутри панели,
      // наружу (за затемнение) не уходит.
      if (e.key === "Tab" && isDrawerOpen() && els.drawer) {
        var focusables = els.drawer.querySelectorAll(
          'button, [href], input, select, textarea, summary, [tabindex]:not([tabindex="-1"])'
        );
        var list = [];
        each(focusables, function (el) {
          if (!el || el.disabled || el.hidden) return;
          if (typeof el.getClientRects === "function" && el.getClientRects().length === 0) return;
          list.push(el);
        });
        if (list.length) {
          var first = list[0];
          var last = list[list.length - 1];
          var active = document.activeElement;
          if (e.shiftKey && (active === first || active === document.body || !els.drawer.contains(active))) {
            e.preventDefault();
            last.focus();
          } else if (!e.shiftKey && active === last) {
            e.preventDefault();
            first.focus();
          }
        }
      }
    });
  }

  function init() {
    bindEvents();
    loadAll(true);
    loadPresets();
    // На телефоне фильтры стартуют свёрнутыми (как на других страницах).
    try {
      if (window.matchMedia && window.matchMedia("(max-width: 768px)").matches) {
        var disc = document.getElementById("svcFilterDisclosure");
        if (disc) disc.removeAttribute("open");
      }
    } catch (e) { /* без matchMedia — раскрыто */ }
  }

  init();
})();