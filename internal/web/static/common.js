// Общие утилиты UI. Подключается на всех страницах ДО страничных скриптов.
// Раньше esc/text/csrf копировались в services.js, dashboard.js, schedule.js
// и app.js — копии расходились (например, правила экранирования).
//
// Внешний контракт: window.mlsUtil.{esc, text, csrf, trim, lower, flag}.
(function () {
  "use strict";

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

  // text — строковое представление значения: объекты сериализуются в JSON,
  // null/undefined дают пустую строку (а не "null"/"undefined").
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

  // flag — флаг MikroTik приходит строкой "true"/"false".
  function flag(value) {
    return value === true || lower(value) === "true";
  }

  // csrf — токен из meta-тега (рендерится в base-шаблоне).
  function csrf() {
    var m = document.querySelector('meta[name="csrf-token"]');
    return m ? m.content : "";
  }

  window.mlsUtil = {
    esc: esc,
    text: text,
    trim: trim,
    lower: lower,
    flag: flag,
    csrf: csrf
  };
})();
