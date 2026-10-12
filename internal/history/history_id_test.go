package history

import "testing"

// TestRecordsByIDFiltersIncarnations проверяет, что записи прежнего
// воплощения сервиса (другой ServiceID) не попадают в выборку текущего.
func TestRecordsByIDFiltersIncarnations(t *testing.T) {
	h := NewHistory(100)
	h.Add(Record{Service: "yt", ServiceID: "old", Added: 1})
	h.Add(Record{Service: "yt", ServiceID: "new", Added: 2})
	h.Add(Record{Service: "yt", ServiceID: "new", Added: 3})
	h.Add(Record{Service: "yt", Added: 4}) // legacy: id не проставлен

	recs := h.RecordsByID("yt", "new", 10)
	if len(recs) != 2 || recs[0].Added != 3 || recs[1].Added != 2 {
		t.Fatalf("id filter broken: %+v", recs)
	}

	last, ok := h.LastByID("yt", "new")
	if !ok || last.Added != 3 {
		t.Fatalf("LastByID broken: %+v ok=%v", last, ok)
	}

	// Пустой id — без фильтра (обратная совместимость со старыми сервисами).
	if recs := h.RecordsByID("yt", "", 10); len(recs) != 4 {
		t.Fatalf("empty id must not filter: %d", len(recs))
	}

	// Другого воплощения записи не отдаются.
	if recs := h.RecordsByID("yt", "old", 10); len(recs) != 1 || recs[0].Added != 1 {
		t.Fatalf("old incarnation leak: %+v", recs)
	}
}
