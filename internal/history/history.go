// Package history — кольцевой буфер последних синхронизаций.
// Хранится в памяти и (опционально) переживает перезапуск через bbolt.
package history

import (
	"encoding/json"
	"sync"
	"time"
)

// Record — одна запись истории.
type Record struct {
	Time       time.Time `json:"time"`
	Service    string    `json:"service"`
	Method     string    `json:"method,omitempty"`
	Status     string    `json:"status,omitempty"`
	Error      string    `json:"error,omitempty"`
	Collected  int       `json:"collected,omitempty"`
	Aggregated int       `json:"aggregated,omitempty"`
	Added      int       `json:"added"`
	Removed    int       `json:"removed"`
	Unchanged  int       `json:"unchanged"`
	DurationMS int64     `json:"duration_ms"`
}

// Store — минимальный интерфейс персистентности (реализует storage.Cache).
type Store interface {
	Put(bucket, key string, v any) error
	Get(bucket, key string, v any) error
}

const bucket = "history"

// History — потокобезопасная история синхронизаций.
type History struct {
	mu      sync.RWMutex
	limit   int
	records []Record
	store   Store
}

// NewHistory создаёт историю размером limit записей (без персистентности).
func NewHistory(limit int) *History {
	if limit <= 0 {
		limit = 1000
	}
	return &History{limit: limit}
}

// AttachStore подключает bbolt и загружает ранее сохранённую историю.
func (h *History) AttachStore(s Store) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.store = s
	var saved []Record
	if s.Get(bucket, "records", &saved) == nil {
		if len(saved) > h.limit {
			saved = saved[len(saved)-h.limit:]
		}
		h.records = saved
	}
}

// Add добавляет запись и (при наличии) сохраняет всё в bbolt.
func (h *History) Add(r Record) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	h.records = append(h.records, r)
	if len(h.records) > h.limit {
		h.records = h.records[len(h.records)-h.limit:]
	}
	if h.store != nil {
		_ = h.store.Put(bucket, "records", h.records)
	}
}

// Records возвращает последние n записей (новые первыми).
// service == "" — все сервисы.
func (h *History) Records(service string, n int) []Record {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if n <= 0 || n > h.limit {
		n = h.limit
	}
	out := make([]Record, 0, n)
	for i := len(h.records) - 1; i >= 0 && len(out) < n; i-- {
		if service != "" && h.records[i].Service != service {
			continue
		}
		out = append(out, h.records[i])
	}
	return out
}

// Last возвращает последнюю запись для сервиса.
func (h *History) Last(service string) (Record, bool) {
	recs := h.Records(service, 1)
	if len(recs) == 0 {
		return Record{}, false
	}
	return recs[0], true
}

// MarshalJSON сериализует всю историю (для API).
func (h *History) MarshalJSON() ([]byte, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return json.Marshal(h.records)
}
