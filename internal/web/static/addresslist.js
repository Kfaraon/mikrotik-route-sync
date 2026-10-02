document.addEventListener('DOMContentLoaded', function() {
    const filters = {
        address: document.getElementById('filterAddress'),
        comment: document.getElementById('filterComment'),
        service: document.getElementById('filterService'),
        managed: document.getElementById('filterManaged'),
        disabled: document.getElementById('filterDisabled')
    };

    const clearBtn = document.getElementById('btnClearFilters');
    if (clearBtn) {
        clearBtn.addEventListener('click', clearFilters);
    }

    function applyFilters() {
        const rows = document.querySelectorAll('#addressTable tbody tr');
        let visibleCount = 0;

        const addressFilter = filters.address ? filters.address.value.toLowerCase() : '';
        const commentFilter = filters.comment ? filters.comment.value.toLowerCase() : '';
        const serviceFilter = filters.service ? filters.service.value.toLowerCase() : '';
        const managedFilter = filters.managed ? filters.managed.value : '';
        const disabledFilter = filters.disabled ? filters.disabled.value : '';

        rows.forEach(function(row) {
            const address = row.dataset.address ? row.dataset.address.toLowerCase() : '';
            const comment = row.dataset.comment ? row.dataset.comment.toLowerCase() : '';
            const service = row.dataset.service ? row.dataset.service.toLowerCase() : '';
            const managed = row.dataset.managed || '';
            const disabled = row.dataset.disabled || '';

            const matchAddress = !addressFilter || address.includes(addressFilter);
            const matchComment = !commentFilter || comment.includes(commentFilter);
            const matchService = !serviceFilter || service.includes(serviceFilter);
            const matchManaged = !managedFilter || managed === managedFilter;
            const matchDisabled = !disabledFilter || disabled === disabledFilter;

            if (matchAddress && matchComment && matchService && matchManaged && matchDisabled) {
                row.style.display = '';
                visibleCount++;
            } else {
                row.style.display = 'none';
            }
        });

        const countEl = document.getElementById('resultsCount');
        if (countEl) {
            countEl.textContent = 'Показано ' + visibleCount + ' из ' + rows.length + ' записей';
        }
    }

    function clearFilters() {
        if (filters.address) filters.address.value = '';
        if (filters.comment) filters.comment.value = '';
        if (filters.service) filters.service.value = '';
        if (filters.managed) filters.managed.value = '';
        if (filters.disabled) filters.disabled.value = '';
        applyFilters();
    }

    // Привязка событий ко всем фильтрам
    Object.keys(filters).forEach(function(key) {
        const input = filters[key];
        if (input) {
            input.addEventListener('input', applyFilters);
        }
    });

    // Инициализация
    applyFilters();
});

// Глобальная функция для уведомлений (может использоваться в других местах)
window.showToast = function(message, type) {
    type = type || 'info';
    const container = document.getElementById('toastContainer');
    if (!container) return;
    
    const toast = document.createElement('div');
    toast.className = 'toast toast-' + type;
    toast.textContent = message;
    container.appendChild(toast);

    setTimeout(function() {
        toast.remove();
    }, 4000);
};
