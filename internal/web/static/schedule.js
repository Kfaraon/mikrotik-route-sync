(function () {
  "use strict";

  var page = document.getElementById("schedule-page");
  if (!page) return;

  var globalSchedule = page.getAttribute("data-global") || "";
  var tbody = document.getElementById("sched-body");
  var rows = tbody
    ? Array.prototype.slice.call(tbody.querySelectorAll(".sched-row"))
    : [];

  var search = document.getElementById("sched-search");
  var sort = document.getElementById("sched-sort");
  var chips = document.getElementById("sched-chips");
  var resetFiltersBtn = document.getElementById("sched-reset-filters");
  var empty = document.getElementById("sched-empty");
  var visibleEl = document.getElementById("sched-visible");

  var summaryEls = {
    total: document.getElementById("sum-total"),
    auto: document.getElementById("sum-auto"),
    manual: document.getElementById("sum-manual"),
    disabled: document.getElementById("sum-disabled"),
    override: document.getElementById("sum-override")
  };

  var activeFilter = "all";

  var weekdays = {
    sunday: 0,
    monday: 1,
    tuesday: 2,
    wednesday: 3,
    thursday: 4,
    friday: 5,
    saturday: 6
  };

  var weekdayRussian = {
    sunday: "воскресеньям",
    monday: "понедельникам",
    tuesday: "вторникам",
    wednesday: "средам",
    thursday: "четвергам",
    friday: "пятницам",
    saturday: "субботам"
  };

  function setText(el, value) {
    if (el) el.textContent = String(value);
  }

  function csrf() {
    var m = document.querySelector('meta[name="csrf-token"]');
    return m ? m.content : "";
  }

  function normalizeSpec(v) {
    return String(v == null ? "" : v).trim();
  }

  function pad(n) {
    return n < 10 ? "0" + n : String(n);
  }

  // Единый стек уведомлений (реализация в app.js: window.mrsToast).
  function showToast(message, isError) {
    if (window.mrsToast) {
      window.mrsToast(message, isError);
    }
  }

  function parseDuration(spec) {
    var m = spec.match(/^every\s+(\d+)\s*(m|h|d|w)$/i);
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

  function classify(spec) {
    var s = normalizeSpec(spec);
    if (!s) {
      return { key: "empty", label: "не задано", className: "badge-muted" };
    }

    var lower = s.toLowerCase();

    if (lower === "manual") {
      return { key: "manual", label: "вручную", className: "badge-manual" };
    }

    if (lower === "disabled") {
      return { key: "disabled", label: "отключено", className: "badge-disabled" };
    }

    if (lower === "inherit") {
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

    var parts = s.split(/\s+/);
    if (parts.length === 5 && /^[\d*\/,\-\s]+$/.test(s)) {
      return { key: "cron", label: "cron", className: "badge-auto" };
    }

    return { key: "custom", label: "своё", className: "badge-auto" };
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
        var day = w[1].toLowerCase();
        var ru = weekdayRussian[day] || day;
        return "по " + ru + " в " + w[2] + ":" + w[3];
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
    if (
      c.key === "manual" ||
      c.key === "disabled" ||
      c.key === "inherit" ||
      c.key === "empty"
    ) {
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
      var dayName = weekly[1].toLowerCase();
      var target = weekdays[dayName];
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

  function formatDateTime(d) {
    if (!d) return "";
    return (
      pad(d.getDate()) +
      "." +
      pad(d.getMonth() + 1) +
      "." +
      d.getFullYear() +
      " " +
      pad(d.getHours()) +
      ":" +
      pad(d.getMinutes())
    );
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

  function effectiveForRow(row) {
    var override = normalizeSpec(row.getAttribute("data-override"));
    if (override) return override;

    var group = normalizeSpec(row.getAttribute("data-group"));
    var groupSchedule = normalizeSpec(row.getAttribute("data-group-schedule"));
    if (group && groupSchedule) return groupSchedule;

    return normalizeSpec(globalSchedule);
  }

  // Потребители читают уже посчитанное эффективное расписание из data-*
  // (его поддерживает updateRow) — единый источник, без второго расчёта.
  function rowEffective(row) {
    return normalizeSpec(row.getAttribute("data-effective"));
  }

  function sourceForRow(row) {
    // Та же логика, что на сервере (атрибут data-source — лишь начальная
    // подсказка): override → своё, группа с непустым расписанием → группа,
    // иначе глобальное. Пересчитывается при правках, поэтому не «залипает».
    if (normalizeSpec(row.getAttribute("data-override"))) return "service";

    var group = normalizeSpec(row.getAttribute("data-group"));
    var groupSchedule = normalizeSpec(row.getAttribute("data-group-schedule"));
    if (group && groupSchedule) return "group";

    return "global";
  }

  function renderSourceBadge(row) {
    var sourceEl = row.querySelector(".source-badge");
    if (!sourceEl) return;

    var source = sourceForRow(row);
    var group = normalizeSpec(row.getAttribute("data-group"));

    var labels = {
      service: "Своё",
      group: group ? "Группа: " + group : "группа",
      global: "Глобальное"
    };
    var classes = {
      service: "source-service",
      group: "source-group",
      global: "source-global"
    };

    sourceEl.textContent = labels[source] || source;
    sourceEl.className = "badge source-badge " + (classes[source] || "");

    // Расписание группы — в подсказке бейджа (отдельной колонки больше нет).
    if (source === "group") {
      var gs = normalizeSpec(row.getAttribute("data-group-schedule"));
      sourceEl.title = gs ? "расписание группы: " + gs : "";
    } else {
      sourceEl.removeAttribute("title");
    }
  }

  function renderNext(row) {
    var nextEl = row.querySelector(".next-badge");
    if (!nextEl) return;

    var eff = rowEffective(row);
    var next = nextRun(eff);

    if (next) {
      nextEl.textContent = "≈ " + formatDateTime(next) + " · " + relativeNext(next);
      nextEl.className = "badge next-badge badge-auto";
    } else {
      nextEl.textContent = "";
      nextEl.className = "badge next-badge";
    }
  }

  function updateRow(row) {
    var override = normalizeSpec(row.getAttribute("data-override"));
    var eff = effectiveForRow(row);

    row.setAttribute("data-effective", eff);
    row.setAttribute("data-source", sourceForRow(row));

    var effEl = row.querySelector(".effective");
    if (effEl) {
      effEl.textContent = humanSchedule(eff);
      effEl.title = eff || "";
    }

    renderSourceBadge(row);
    renderNext(row);

    var panel = row.querySelector(".edit-panel");
    if (panel && panel.hidden) {
      // Редактор закрыт — просто синхронизируем значения в контролах.
      var inputHidden = row.querySelector(".schedule-input");
      if (inputHidden && document.activeElement !== inputHidden) {
        inputHidden.value = override;
      }
    }

    var input = row.querySelector(".schedule-input");
    if (input) {
      input.placeholder = eff ? humanSchedule(eff) : "укажите расписание";
    }

    var preset = row.querySelector(".preset");
    if (preset) {
      var wanted = override || "inherit";
      var matched = false;

      for (var i = 0; i < preset.options.length; i++) {
        if (preset.options[i].value === wanted) {
          matched = true;
          break;
        }
      }

      preset.value = matched ? wanted : "custom";
    }
  }

  function updateSummary() {
    var counts = {
      total: rows.length,
      auto: 0,
      manual: 0,
      disabled: 0,
      override: 0
    };

    rows.forEach(function (row) {
      var eff = rowEffective(row);
      var c = classify(eff);

      if (c.key === "manual") {
        counts.manual++;
      } else if (c.key === "disabled") {
        counts.disabled++;
      } else if (
        c.key === "interval" ||
        c.key === "daily" ||
        c.key === "weekly" ||
        c.key === "cron" ||
        c.key === "custom"
      ) {
        counts.auto++;
      }

      if (normalizeSpec(row.getAttribute("data-override"))) {
        counts.override++;
      }
    });

    setText(summaryEls.total, counts.total);
    setText(summaryEls.auto, counts.auto);
    setText(summaryEls.manual, counts.manual);
    setText(summaryEls.disabled, counts.disabled);
    setText(summaryEls.override, counts.override);
  }

  function rowMatches(row) {
    var term = normalizeSpec(search && search.value).toLowerCase();

    var service = normalizeSpec(row.getAttribute("data-service"));
    var eff = rowEffective(row);
    var override = normalizeSpec(row.getAttribute("data-override"));
    var c = classify(eff);

    if (term) {
      var hay = [
        service,
        normalizeSpec(row.getAttribute("data-group")),
        eff,
        override
      ]
        .join(" ")
        .toLowerCase();
      if (hay.indexOf(term) === -1) return false;
    }

    if (activeFilter === "auto") {
      return (
        c.key === "interval" ||
        c.key === "daily" ||
        c.key === "weekly" ||
        c.key === "cron" ||
        c.key === "custom"
      );
    }

    if (activeFilter === "manual") return c.key === "manual";
    if (activeFilter === "disabled") return c.key === "disabled";
    if (activeFilter === "override") return !!override;

    return true;
  }

  function sortRows() {
    var mode = (sort && sort.value) || "name";
    var sorted = rows.slice();

    sorted.sort(function (a, b) {
      var as = normalizeSpec(a.getAttribute("data-service"));
      var bs = normalizeSpec(b.getAttribute("data-service"));

      if (mode === "name") {
        return as.localeCompare(bs);
      }

      if (mode === "schedule") {
        var ae = rowEffective(a);
        var be = rowEffective(b);
        return ae.localeCompare(be) || as.localeCompare(bs);
      }

      if (mode === "source") {
        var order = { service: 0, group: 1, global: 2 };
        var ao = order[sourceForRow(a)] || 99;
        var bo = order[sourceForRow(b)] || 99;
        return (ao - bo) || as.localeCompare(bs);
      }

      if (mode === "next") {
        var an = nextRun(rowEffective(a));
        var bn = nextRun(rowEffective(b));

        if (!an && !bn) return as.localeCompare(bs);
        if (!an) return 1;
        if (!bn) return -1;
        return an.getTime() - bn.getTime() || as.localeCompare(bs);
      }

      return 0;
    });

    sorted.forEach(function (row) {
      if (tbody) tbody.appendChild(row);
    });
  }

  function applyViews() {
    sortRows();

    var visible = 0;

    rows.forEach(function (row) {
      var match = rowMatches(row);
      row.style.display = match ? "" : "none";
      if (match) visible++;
    });

    setText(visibleEl, "Показано " + visible + " из " + rows.length);

    if (empty) {
      if (rows.length === 0) {
        empty.hidden = false;
        empty.textContent = "Нет сервисов. Добавьте сервис на странице «Сервисы».";
      } else if (visible === 0) {
        empty.hidden = false;
        empty.textContent = "Ничего не найдено. Измените поиск или фильтр.";
      } else {
        empty.hidden = true;
      }
    }
  }

  function setStatus(row, message, kind) {
    var el = row.querySelector(".row-status");
    if (!el) return;

    el.textContent = message || "";
    el.className = "row-status" + (kind ? " " + kind : "");
  }

  function setChip(filter) {
    activeFilter = filter || "all";
    if (!chips) return;
    var all = chips.querySelectorAll(".sched-chip");
    Array.prototype.forEach.call(all, function (chip) {
      var on = chip.getAttribute("data-filter") === activeFilter;
      chip.classList.toggle("active", on);
      chip.setAttribute("aria-pressed", on ? "true" : "false");
    });
  }

  function closeEditors(except) {
    rows.forEach(function (row) {
      if (row === except) return;
      var panel = row.querySelector(".edit-panel");
      var btn = row.querySelector(".edit-open-btn");
      if (panel && !panel.hidden) {
        panel.hidden = true;
        if (btn) {
          btn.setAttribute("aria-expanded", "false");
          btn.textContent = "Изменить";
        }
      }
    });
  }

  function toggleEditor(row) {
    var panel = row.querySelector(".edit-panel");
    var btn = row.querySelector(".edit-open-btn");
    if (!panel) return;

    var willOpen = panel.hidden;
    closeEditors(row);
    panel.hidden = !willOpen;

    if (btn) {
      btn.setAttribute("aria-expanded", willOpen ? "true" : "false");
      btn.textContent = willOpen ? "Свернуть" : "Изменить";
    }

    if (willOpen) {
      var input = row.querySelector(".schedule-input");
      if (input) {
        input.focus();
        input.select();
      }
    }
  }

  function saveSchedule(row, value) {
    var service = normalizeSpec(row.getAttribute("data-service"));
    var spec = normalizeSpec(value);

    if (!service) return;

    setStatus(row, "Сохранение…", "muted");
    row.classList.add("saving");

    fetch("/api/v1/schedules/" + encodeURIComponent(service), {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
        "X-CSRF-Token": csrf()
      },
      credentials: "same-origin",
      body: JSON.stringify({ schedule: spec })
    })
      .then(function (response) {
        return response.json().then(function (data) {
          return {
            ok: response.ok && data && data.ok,
            status: response.status,
            data: data
          };
        });
      })
      .then(function (result) {
        row.classList.remove("saving");

        if (!result.ok) {
          var msg =
            result.data && result.data.error
              ? result.data.error
              : "HTTP " + result.status;
          throw new Error(msg);
        }

        row.setAttribute("data-override", spec);
        updateRow(row);
        applyViews();
        updateSummary();

        setStatus(
          row,
          spec ? "Сохранено: " + humanSchedule(spec) : "Наследуется из группы/глобально",
          "ok"
        );

        row.classList.add("saved");
        setTimeout(function () {
          row.classList.remove("saved");
        }, 1400);

        closeEditors();
        showToast("Расписание " + service + " обновлено");
      })
      .catch(function (err) {
        row.classList.remove("saving");

        var msg = err && err.message ? err.message : String(err);
        setStatus(row, "Ошибка: " + msg, "err");
        showToast("Ошибка сохранения: " + msg, true);
      });
  }

  // --- Группы ---

  function scopeValue(scopeRow) {
    var code = scopeRow.querySelector(".group-value");
    return code ? normalizeSpec(code.textContent) : "";
  }

  function setScopeStatus(scopeRow, message, kind) {
    var el = scopeRow.querySelector(".scope-status");
    if (!el) return;
    el.textContent = message || "";
    el.className = "scope-status" + (kind ? " " + kind : "");
  }

  function toggleScopeEditor(scopeRow) {
    var panel = scopeRow.querySelector(".scope-editor");
    var btn = scopeRow.querySelector(".scope-edit-btn");
    if (!panel) return;

    var willOpen = panel.hidden;
    panel.hidden = !willOpen;
    if (btn) btn.textContent = willOpen ? "Свернуть" : "Изменить";

    if (willOpen) {
      var input = panel.querySelector(".scope-input");
      if (input) {
        input.value = scopeValue(scopeRow);
        input.focus();
        input.select();
      }
    }
  }

  function saveScope(scopeRow) {
    var panel = scopeRow.querySelector(".scope-editor");
    var input = panel ? panel.querySelector(".scope-input") : null;
    var spec = normalizeSpec(input ? input.value : "");
    var group = normalizeSpec(scopeRow.getAttribute("data-group"));

    var url = "/api/v1/schedules/group/" + encodeURIComponent(group);

    setScopeStatus(scopeRow, "Сохранение…", "muted");

    fetch(url, {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
        "X-CSRF-Token": csrf()
      },
      credentials: "same-origin",
      body: JSON.stringify({ schedule: spec })
    })
      .then(function (response) {
        return response.json().then(function (data) {
          return { ok: response.ok && data && data.ok, status: response.status, data: data };
        });
      })
      .then(function (result) {
        if (!result.ok) {
          var msg = result.data && result.data.error ? result.data.error : "HTTP " + result.status;
          throw new Error(msg);
        }

        var code = scopeRow.querySelector(".group-value");
        if (code) code.textContent = spec;
        // Обновляем расписание группы у затронутых строк и пересчитываем.
        rows.forEach(function (row) {
          if (normalizeSpec(row.getAttribute("data-group")) === group) {
            row.setAttribute("data-group-schedule", spec);
          }
        });

        rows.forEach(updateRow);
        applyViews();
        updateSummary();

        setScopeStatus(scopeRow, "Сохранено", "ok");
        closeScopeEditor(scopeRow);
        showToast("Расписание сохранено");
      })
      .catch(function (err) {
        var msg = err && err.message ? err.message : String(err);
        setScopeStatus(scopeRow, "Ошибка: " + msg, "err");
        showToast("Ошибка сохранения: " + msg, true);
      });
  }

  function closeScopeEditor(scopeRow) {
    var panel = scopeRow.querySelector(".scope-editor");
    var btn = scopeRow.querySelector(".scope-edit-btn");
    if (panel) panel.hidden = true;
    if (btn) btn.textContent = "Изменить";
  }

  function bindScope(scopeRow) {
    var editBtn = scopeRow.querySelector(".scope-edit-btn");
    var saveBtn = scopeRow.querySelector(".scope-save-btn");
    var cancelBtn = scopeRow.querySelector(".scope-cancel-btn");
    var preset = scopeRow.querySelector(".preset");
    var input = scopeRow.querySelector(".scope-input");

    if (editBtn) {
      editBtn.addEventListener("click", function () {
        toggleScopeEditor(scopeRow);
      });
    }

    if (preset && input) {
      preset.addEventListener("change", function () {
        var v = preset.value;
        if (v === "custom") {
          input.focus();
          return;
        }
        if (v === "") return;
        input.value = v;
      });
    }

    if (input) {
      input.addEventListener("keydown", function (e) {
        if (e.key === "Enter") {
          e.preventDefault();
          saveScope(scopeRow);
        }
      });
    }

    if (saveBtn) {
      saveBtn.addEventListener("click", function () {
        saveScope(scopeRow);
      });
    }

    if (cancelBtn) {
      cancelBtn.addEventListener("click", function () {
        closeScopeEditor(scopeRow);
        setScopeStatus(scopeRow, "", "");
      });
    }
  }

  function bindRow(row) {
    var preset = row.querySelector(".preset");
    var input = row.querySelector(".schedule-input");
    var saveBtn = row.querySelector(".save-btn");
    var resetBtn = row.querySelector(".reset-btn");
    var openBtn = row.querySelector(".edit-open-btn");

    if (openBtn) {
      openBtn.addEventListener("click", function (e) {
        e.preventDefault();
        toggleEditor(row);
      });
    }

    if (preset && input) {
      preset.addEventListener("change", function () {
        var value = preset.value;

        if (value === "custom") {
          input.focus();
          return;
        }

        if (value === "") return;

        input.value = value === "inherit" ? "" : value;
        saveSchedule(row, input.value);
      });
    }

    if (input) {
      input.addEventListener("keydown", function (e) {
        if (e.key === "Enter") {
          e.preventDefault();
          saveSchedule(row, input.value);
        }
      });
    }

    if (saveBtn) {
      saveBtn.addEventListener("click", function () {
        saveSchedule(row, input ? input.value : "");
      });
    }

    if (resetBtn) {
      resetBtn.addEventListener("click", function () {
        var current = normalizeSpec(row.getAttribute("data-override"));
        var service = normalizeSpec(row.getAttribute("data-service"));

        if (!current) {
          setStatus(row, "Переопределения нет", "muted");
          return;
        }

        if (!window.confirm("Сбросить переопределение для " + service + "?")) {
          return;
        }

        if (input) input.value = "";
        if (preset) preset.value = "inherit";

        saveSchedule(row, "");
      });
    }
  }

  function bindGlobal() {
    if (search) {
      var timer = null;

      search.addEventListener("input", function () {
        clearTimeout(timer);
        timer = setTimeout(applyViews, 180);
      });
    }

    if (chips) {
      chips.addEventListener("click", function (e) {
        var chip = e.target.closest ? e.target.closest(".sched-chip") : null;
        if (!chip) return;
        var wanted = chip.getAttribute("data-filter");
        // Повторный клик по активному чипу (кроме «Все») — сброс на «Все».
        if (wanted === activeFilter && wanted !== "all") {
          setChip("all");
        } else {
          setChip(wanted);
        }
        applyViews();
      });
    }

    if (sort) {
      sort.addEventListener("change", applyViews);
    }

    if (resetFiltersBtn) {
      resetFiltersBtn.addEventListener("click", function () {
        if (search) search.value = "";
        if (sort) sort.value = "name";
        setChip("all");
        applyViews();
      });
    }

    document.addEventListener("keydown", function (e) {
      var tag = document.activeElement && document.activeElement.tagName;
      var typing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";

      if (e.key === "/" && !typing && search) {
        e.preventDefault();
        search.focus();
      }

      if (e.key === "Escape") {
        if (document.activeElement === search && search.value) {
          search.value = "";
          applyViews();
        }
        closeEditors();
      }
    });
  }

  function init() {
    rows.forEach(updateRow);
    updateSummary();
    applyViews();
    rows.forEach(bindRow);

    var scopeRows = page.querySelectorAll(".global-row");
    Array.prototype.forEach.call(scopeRows, bindScope);

    bindGlobal();

    // next-run пересчитываем раз в минуту: относительное время устаревает.
    setInterval(function () {
      rows.forEach(renderNext);
    }, 60000);
  }

  init();
})();
