package core

// Тесты подсказок доменов (domainsuggest.go): httptest-стаб urlscan.io
// (search → UUID, result → lists.domains), офлайн, без реальной сети.
// Пакетное состояние (кэш, пауза 429, базовый URL) сбрасывается в каждом
// тесте — кейсы идут последовательно, без t.Parallel.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// suggestStub — стаб api urlscan.io. Ловит лишние HTTP-запросы
// (хит-счётчики), проверяет заголовок API-Key и пишет запросы поиска.
type suggestStub struct {
	t       *testing.T
	wantKey string // ожидаемый API-Key ("" — заголовок запрещён)

	hits         atomic.Int64 // всего HTTP-запросов
	searches     atomic.Int64 // запросов поиска
	searchStatus int          // !=0 → статус ответа search
	searchBody   string
	resultStatus map[string]int    // uuid → статус (404 и т.п.)
	resultBody   map[string]string // uuid → JSON

	mu      sync.Mutex
	queries []url.Values
}

func (st *suggestStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st.hits.Add(1)
	if got := r.Header.Get("API-Key"); got != st.wantKey {
		st.t.Errorf("API-Key=%q, want %q", got, st.wantKey)
		http.Error(w, "unexpected api key", http.StatusForbidden)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/search/"):
		st.searches.Add(1)
		st.mu.Lock()
		st.queries = append(st.queries, r.URL.Query())
		st.mu.Unlock()
		if st.searchStatus != 0 {
			w.WriteHeader(st.searchStatus)
			return
		}
		_, _ = w.Write([]byte(st.searchBody))
	case strings.HasPrefix(r.URL.Path, "/api/v1/result/"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/result/"), "/")
		if code := st.resultStatus[id]; code != 0 {
			w.WriteHeader(code)
			return
		}
		body, ok := st.resultBody[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	default:
		http.NotFound(w, r)
	}
}

// firstQuery — параметры первого запроса поиска (q, size).
func (st *suggestStub) firstQuery(t *testing.T) url.Values {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.queries) == 0 {
		t.Fatal("no search query hit the stub")
	}
	return st.queries[0]
}

// newSuggestStub — стаб + подмена urlscanBaseURL (шов для тестов) и
// сброс пакетного состояния (кэш, пауза 429).
func newSuggestStub(t *testing.T, st *suggestStub) {
	t.Helper()
	suggestStateMu.Lock()
	suggestCache = map[string]suggestCacheEntry{}
	suggestCoolUntil = time.Time{}
	suggestStateMu.Unlock()
	orig := urlscanBaseURL
	t.Cleanup(func() { urlscanBaseURL = orig })

	srv := httptest.NewServer(st)
	t.Cleanup(srv.Close)
	urlscanBaseURL = srv.URL
}

// suggestSearchBody — JSON ответа поиска с UUID.
func suggestSearchBody(ids ...string) string {
	var b strings.Builder
	b.WriteString(`{"results":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"_id":%q}`, id)
	}
	fmt.Fprintf(&b, `],"total":%d}`, len(ids))
	return b.String()
}

func TestSuggestDomainsOff(t *testing.T) {
	st := &suggestStub{t: t, searchBody: suggestSearchBody("uuid-1")}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "off"

	res := s.SuggestDomains(context.Background(), "example.com", 10)
	if res == nil {
		t.Fatal("nil result — совет обязан возвращаться всегда")
	}
	if res.Enabled {
		t.Fatalf("off must be disabled: %+v", res)
	}
	if res.Reason == "" {
		t.Fatal("disabled result must carry reason")
	}
	if res.Mode != "off" {
		t.Fatalf("mode=%q, want off", res.Mode)
	}
	if st.hits.Load() != 0 {
		t.Fatalf("off must not hit network, hits=%d", st.hits.Load())
	}
}

func TestSuggestDomainsKeyWithoutKey(t *testing.T) {
	st := &suggestStub{t: t}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "key" // ключ не задан
	s.cfg.External.UrlscanAPIKey = ""

	res := s.SuggestDomains(context.Background(), "example.com", 10)
	if res == nil {
		t.Fatal("nil result — совет обязан возвращаться всегда")
	}
	if res.Enabled {
		t.Fatalf("key without api key must be disabled: %+v", res)
	}
	if !strings.Contains(res.Reason, "urlscan_api_key") {
		t.Fatalf("reason %q must name the missing key", res.Reason)
	}
	if st.hits.Load() != 0 {
		t.Fatalf("must fail fast without network, hits=%d", st.hits.Load())
	}
}

func TestSuggestDomainsSuccess(t *testing.T) {
	st := &suggestStub{
		t:          t,
		searchBody: suggestSearchBody("uuid-1", "uuid-2"),
		resultBody: map[string]string{
			"uuid-1": `{"page":{"domain":"example.com"},"lists":{"domains":[` +
				`"static.example.com","cdn.example.com","www.google-analytics.com","example.com"]}}`,
			"uuid-2": `{"page":{"domain":"example.com"},"lists":{"domains":[` +
				`"static.example.com","img.example.com","example.com"]}}`,
		},
	}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "anonymous"

	// Имя с мусором проверяет нормализацию запроса.
	res := s.SuggestDomains(context.Background(), "Example.COM.", 100)
	if res == nil {
		t.Fatal("nil result")
	}
	if !res.Enabled {
		t.Fatalf("want enabled: %+v", res)
	}
	if res.Cached {
		t.Fatal("first call must not be cached")
	}
	if res.Mode != "anonymous" || res.Source != "urlscan" {
		t.Fatalf("mode=%q source=%q", res.Mode, res.Source)
	}
	if res.Query != "domain:example.com date:>now-30d" {
		t.Fatalf("query=%q", res.Query)
	}
	if res.ScansConsidered != 2 {
		t.Fatalf("scans considered=%d, want 2", res.ScansConsidered)
	}

	q := st.firstQuery(t)
	if q.Get("q") != res.Query {
		t.Fatalf("search q=%q, want %q", q.Get("q"), res.Query)
	}
	if q.Get("size") != "10" {
		t.Fatalf("size=%q, want cap 10 (maxScans=100)", q.Get("size"))
	}
	if st.hits.Load() != 3 { // 1 search + 2 result
		t.Fatalf("hits=%d, want 3", st.hits.Load())
	}

	find := func(dom string) (DomainSuggestion, bool) {
		for _, c := range res.Candidates {
			if c.Domain == dom {
				return c, true
			}
		}
		return DomainSuggestion{}, false
	}
	if len(res.Candidates) != 3 {
		t.Fatalf("candidates=%v, want 3", res.Candidates)
	}
	top, ok := find("static.example.com")
	if !ok {
		t.Fatalf("static.example.com missing: %+v", res.Candidates)
	}
	if top.Scans != 2 || top.Requests != 2 || top.Confidence != "high" {
		t.Fatalf("static: scans=%d requests=%d confidence=%q", top.Scans, top.Requests, top.Confidence)
	}
	if top.Apex != "example.com" {
		t.Fatalf("apex=%q, want example.com", top.Apex)
	}
	if res.Candidates[0].Domain != "static.example.com" {
		t.Fatalf("best candidate must be first: %v", res.Candidates)
	}
	for _, dom := range []string{"cdn.example.com", "img.example.com"} {
		c, ok := find(dom)
		if !ok {
			t.Fatalf("%s missing: %+v", dom, res.Candidates)
		}
		if c.Scans != 1 || c.Confidence != "low" {
			t.Fatalf("%s: scans=%d confidence=%q, want 1/low", dom, c.Scans, c.Confidence)
		}
	}

	// Apex запроса и трекер не должны попасть в кандидаты.
	for _, c := range res.Candidates {
		if c.Domain == "example.com" || strings.Contains(c.Domain, "google-analytics") {
			t.Fatalf("filtered domain leaked: %+v", c)
		}
	}
	excluded := map[string]string{}
	for _, e := range res.Excluded {
		excluded[e.Domain] = e.Reason
	}
	if r, ok := excluded["example.com"]; !ok || r == "" {
		t.Fatalf("apex must be excluded with reason: %+v", res.Excluded)
	}
	if r, ok := excluded["www.google-analytics.com"]; !ok || r == "" {
		t.Fatalf("tracker must be excluded with reason: %+v", res.Excluded)
	}
}

func TestSuggestDomainsKeyModeSendsHeader(t *testing.T) {
	st := &suggestStub{
		t:          t,
		wantKey:    "k-123", // стаб упадёт, если заголовка не будет
		searchBody: suggestSearchBody("uuid-1"),
		resultBody: map[string]string{
			"uuid-1": `{"lists":{"domains":["cdn.example.com"]}}`,
		},
	}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "key"
	s.cfg.External.UrlscanAPIKey = "k-123"

	res := s.SuggestDomains(context.Background(), "example.com", 5)
	if res == nil || !res.Enabled {
		t.Fatalf("want enabled result: %+v", res)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Domain != "cdn.example.com" {
		t.Fatalf("candidates=%v", res.Candidates)
	}
	if st.searches.Load() != 1 {
		t.Fatalf("searches=%d, want 1", st.searches.Load())
	}
}

func TestSuggestDomainsTrackerExcluded(t *testing.T) {
	st := &suggestStub{
		t:          t,
		searchBody: suggestSearchBody("uuid-1"),
		resultBody: map[string]string{
			"uuid-1": `{"lists":{"domains":[` +
				`"example.com","www.googletagmanager.com","connect.facebook.net",` +
				`"sub.doubleclick.net","stats.g.doubleclick.net","keep.example.com"]}}`,
		},
	}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "anonymous"

	res := s.SuggestDomains(context.Background(), "example.com", 10)
	if res == nil || !res.Enabled {
		t.Fatalf("want enabled result: %+v", res)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Domain != "keep.example.com" {
		t.Fatalf("candidates=%v, want only keep.example.com", res.Candidates)
	}

	wantExcluded := []string{
		"example.com", // apex запроса
		"www.googletagmanager.com",
		"connect.facebook.net",
		"sub.doubleclick.net",
		"stats.g.doubleclick.net",
	}
	excluded := map[string]string{}
	for _, e := range res.Excluded {
		excluded[e.Domain] = e.Reason
	}
	for _, dom := range wantExcluded {
		r, ok := excluded[dom]
		if !ok {
			t.Fatalf("%s must be excluded: %+v", dom, res.Excluded)
		}
		if r == "" {
			t.Fatalf("excluded %s must carry reason", dom)
		}
	}
	if len(excluded) != len(wantExcluded) {
		t.Fatalf("excluded=%v, want exactly %d", res.Excluded, len(wantExcluded))
	}
}

func TestSuggestDomainsRateLimited(t *testing.T) {
	st := &suggestStub{t: t, searchStatus: http.StatusTooManyRequests}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "anonymous"

	res := s.SuggestDomains(context.Background(), "example.com", 10)
	if res == nil {
		t.Fatal("nil result")
	}
	if res.Enabled {
		t.Fatalf("429 must disable: %+v", res)
	}
	if res.Reason != "rate limited" {
		t.Fatalf("reason=%q, want %q", res.Reason, "rate limited")
	}

	// Пауза запомнена: повторный вызов не должен снова стучаться в сеть.
	res2 := s.SuggestDomains(context.Background(), "example.com", 10)
	if res2 == nil || res2.Enabled || res2.Reason != "rate limited" {
		t.Fatalf("cooldown call: %+v", res2)
	}
	if st.hits.Load() != 1 {
		t.Fatalf("hits=%d, want 1 (cooldown blocks further calls)", st.hits.Load())
	}
}

func TestSuggestDomainsCache(t *testing.T) {
	st := &suggestStub{
		t:          t,
		searchBody: suggestSearchBody("uuid-1"),
		resultBody: map[string]string{
			"uuid-1": `{"lists":{"domains":["static.example.com","example.com"]}}`,
		},
	}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "anonymous"
	ctx := context.Background()

	res1 := s.SuggestDomains(ctx, "example.com", 10)
	if res1 == nil || !res1.Enabled {
		t.Fatalf("first call: %+v", res1)
	}
	if res1.Cached {
		t.Fatal("first call must not be cached")
	}
	hitsAfterFirst := st.hits.Load()
	if hitsAfterFirst == 0 {
		t.Fatal("first call must hit the stub")
	}

	res2 := s.SuggestDomains(ctx, "example.com", 10)
	if res2 == nil || !res2.Cached {
		t.Fatalf("second call must come from cache: %+v", res2)
	}
	if st.hits.Load() != hitsAfterFirst {
		t.Fatalf("cache hit must not do HTTP: hits %d -> %d", hitsAfterFirst, st.hits.Load())
	}
	if !res2.FetchedAt.Equal(res1.FetchedAt) {
		t.Fatalf("cached FetchedAt=%v, want original %v", res2.FetchedAt, res1.FetchedAt)
	}
	if len(res2.Candidates) != len(res1.Candidates) {
		t.Fatalf("cached candidates=%d, want %d", len(res2.Candidates), len(res1.Candidates))
	}

	// Мутация первого ответа не портит кэш (в кэше лежит копия).
	res1.Candidates[0].Domain = "corrupted.example.net"
	res3 := s.SuggestDomains(ctx, "example.com", 10)
	if res3 == nil || !res3.Cached {
		t.Fatalf("third call: %+v", res3)
	}
	if res3.Candidates[0].Domain != "static.example.com" {
		t.Fatalf("cache corrupted: %q", res3.Candidates[0].Domain)
	}
}

func TestSuggestDomainsCacheTTL(t *testing.T) {
	run := func(t *testing.T, mode, apiKey string, wantCached bool) {
		t.Helper()
		st := &suggestStub{
			t:          t,
			wantKey:    apiKey,
			searchBody: suggestSearchBody("uuid-1"),
			resultBody: map[string]string{
				"uuid-1": `{"lists":{"domains":["a.example.com"]}}`,
			},
		}
		newSuggestStub(t, st)
		s, _ := newTestSyncer(t, noopRouter)
		s.cfg.Suggest.UrlscanMode = mode
		s.cfg.External.UrlscanAPIKey = apiKey
		ctx := context.Background()

		if res := s.SuggestDomains(ctx, "ttl.example.com", 5); res == nil || res.Cached {
			t.Fatalf("first call: %+v", res)
		}
		// Старим единственную запись кэша на 7 часов: anonymous (TTL 24ч)
		// ещё держит, key (TTL 6ч) — уже нет.
		suggestStateMu.Lock()
		for k, ent := range suggestCache {
			ent.at = ent.at.Add(-7 * time.Hour)
			suggestCache[k] = ent
		}
		suggestStateMu.Unlock()

		res := s.SuggestDomains(ctx, "ttl.example.com", 5)
		if res == nil {
			t.Fatal("nil result")
		}
		if res.Cached != wantCached {
			t.Fatalf("cached=%v after 7h in %s mode, want %v", res.Cached, mode, wantCached)
		}
	}

	t.Run("anonymous holds 24h", func(t *testing.T) {
		run(t, "anonymous", "", true)
	})
	t.Run("key expires at 6h", func(t *testing.T) {
		run(t, "key", "k-ttl", false)
	})
}

func TestSuggestDomainsSkipsMissingScan(t *testing.T) {
	st := &suggestStub{
		t:          t,
		searchBody: suggestSearchBody("uuid-gone", "uuid-ok"),
		resultStatus: map[string]int{
			"uuid-gone": http.StatusNotFound,
		},
		resultBody: map[string]string{
			"uuid-ok": `{"lists":{"domains":["keep.example.com"]}}`,
		},
	}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "anonymous"

	res := s.SuggestDomains(context.Background(), "example.com", 10)
	if res == nil || !res.Enabled {
		t.Fatalf("404 on one scan must not kill the advice: %+v", res)
	}
	if res.ScansConsidered != 1 {
		t.Fatalf("scans considered=%d, want 1 (uuid-gone skipped)", res.ScansConsidered)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Domain != "keep.example.com" {
		t.Fatalf("candidates=%v", res.Candidates)
	}
}

func TestSuggestDomainsUmbrellaMedium(t *testing.T) {
	st := &suggestStub{
		t:          t,
		searchBody: suggestSearchBody("uuid-u"),
		resultBody: map[string]string{
			"uuid-u": `{"page":{"domain":"top.example.com"},"umbrella":{"rank":123},` +
				`"lists":{"domains":["top.example.com"]}}`,
		},
	}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "anonymous"

	res := s.SuggestDomains(context.Background(), "example.com", 10)
	if res == nil || !res.Enabled {
		t.Fatalf("want enabled: %+v", res)
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates=%v", res.Candidates)
	}
	c := res.Candidates[0]
	if c.Domain != "top.example.com" || c.Scans != 1 {
		t.Fatalf("candidate=%+v, want single scan of top.example.com", c)
	}
	if c.Confidence != "medium" {
		t.Fatalf("confidence=%q, want medium (umbrella top, 1 scan)", c.Confidence)
	}
}

func TestSuggestDomainsNoScans(t *testing.T) {
	st := &suggestStub{t: t, searchBody: `{"results":[],"total":0}`}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "anonymous"

	res := s.SuggestDomains(context.Background(), "fresh.example.com", 10)
	if res == nil {
		t.Fatal("nil result")
	}
	if !res.Enabled {
		t.Fatalf("empty search is a valid answer: %+v", res)
	}
	if len(res.Candidates) != 0 || res.ScansConsidered != 0 {
		t.Fatalf("candidates=%v scans=%d", res.Candidates, res.ScansConsidered)
	}
	if res.Reason == "" {
		t.Fatal("empty result must carry reason")
	}
	if st.hits.Load() != 1 {
		t.Fatalf("hits=%d, want 1", st.hits.Load())
	}
}

func TestSuggestDomainsCanceledContext(t *testing.T) {
	st := &suggestStub{t: t, searchBody: suggestSearchBody("uuid-1")}
	newSuggestStub(t, st)
	s, _ := newTestSyncer(t, noopRouter)
	s.cfg.Suggest.UrlscanMode = "anonymous"

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := s.SuggestDomains(ctx, "example.com", 5)
	if res == nil {
		t.Fatal("nil result — совет обязан возвращаться всегда")
	}
	if res.Enabled {
		t.Fatalf("canceled context must not enable: %+v", res)
	}
	if res.Reason == "" {
		t.Fatal("canceled context must carry reason")
	}
	if st.hits.Load() != 0 {
		t.Fatalf("canceled context must not hit network, hits=%d", st.hits.Load())
	}
}
