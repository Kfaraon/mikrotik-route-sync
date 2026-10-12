(function () {
    "use strict";

    var table = document.getElementById("addressTable");
    if (!table) return;

    var filters = {
        address: document.getElementById("filterAddress"),
        comment: document.getElementById("filterComment"),
        service: document.getElementById("filterService"),
        managed: document.getElementById("filterManaged"),
        disabled: document.getElementById("filterDisabled")
    };

    var clearBtn = document.getElementById("btnClearFilters");
    var emptyBox = document.getElementById("address-empty");
    var emptyResetBtn = document.getElementById("btnResetFiltersEmpty");

    function applyFilters() {
        var rows = document.querySelectorAll("#addressTable tbody tr");
        var visibleCount = 0;

        var addressFilter = filters.address ? filters.address.value.toLowerCase() : "";
        var commentFilter = filters.comment ? filters.comment.value.toLowerCase() : "";
        var serviceFilter = filters.service ? filters.service.value.toLowerCase() : "";
        var managedFilter = filters.managed ? filters.managed.value : "";
        var disabledFilter = filters.disabled ? filters.disabled.value : "";

        for (var i = 0; i < rows.length; i++) {
            var row = rows[i];
            var address = (row.dataset.address || "").toLowerCase();
            var comment = (row.dataset.comment || "").toLowerCase();
            var service = (row.dataset.service || "").toLowerCase();
            var managed = row.dataset.managed || "";
            var disabled = row.dataset.disabled || "";

            var match = (!addressFilter || address.indexOf(addressFilter) !== -1) &&
                        (!commentFilter || comment.indexOf(commentFilter) !== -1) &&
                        (!serviceFilter || service.indexOf(serviceFilter) !== -1) &&
                        (!managedFilter || managed === managedFilter) &&
                        (!disabledFilter || disabled === disabledFilter);

            row.style.display = match ? "" : "none";
            if (match) visibleCount++;
        }

        var countEl = document.getElementById("resultsCount");
        if (countEl) {
            countEl.textContent = "Показано " + visibleCount + " из " + rows.length + " записей";
        }

        // Пусто после фильтрации — подсказка со сбросом вместо голой таблицы.
        if (emptyBox) {
            emptyBox.hidden = visibleCount !== 0;
        }
    }

    function clearFilters() {
        if (filters.address) filters.address.value = "";
        if (filters.comment) filters.comment.value = "";
        if (filters.service) filters.service.value = "";
        if (filters.managed) filters.managed.value = "";
        if (filters.disabled) filters.disabled.value = "";
        applyFilters();
    }

    if (clearBtn) {
        clearBtn.addEventListener("click", clearFilters);
    }

    if (emptyResetBtn) {
        emptyResetBtn.addEventListener("click", clearFilters);
    }

    for (var key in filters) {
        if (filters[key]) {
            filters[key].addEventListener("input", applyFilters);
        }
    }

    // Карточки сворачиваются: видны адрес и комментарий, тап разворачивает.
    // Делегирование — строки серверные, класс переживает фильтрацию.
    (function () {
        var body = table.querySelector("tbody");
        if (!body) return;

        function toggle(tr) {
            if (!tr || tr.parentNode !== body) return;
            var open = tr.classList.toggle("open");
            tr.setAttribute("aria-expanded", open ? "true" : "false");
        }

        var rows = body.querySelectorAll("tr");
        for (var i = 0; i < rows.length; i++) {
            rows[i].setAttribute("tabindex", "0");
            rows[i].setAttribute("role", "button");
            rows[i].setAttribute("aria-expanded", "false");
        }

        body.addEventListener("click", function (e) {
            var tr = e.target && e.target.closest ? e.target.closest("tr") : null;
            toggle(tr);
        });

        body.addEventListener("keydown", function (e) {
            if (e.key !== "Enter" && e.key !== " ") return;
            var tr = e.target && e.target.closest ? e.target.closest("tr") : null;
            if (!tr) return;
            e.preventDefault();
            toggle(tr);
        });
    })();

    applyFilters();
})();
