package core

// Подсказки доменов по urlscan.io (советник, не синк): поиск сканов за
// 30 дней по домену сервиса, разбор lists.domains у до 5 UUID, группировка
// и оценка уверенности. Как и AdviseCDN — совет без побочных эффектов:
// никогда не паникует, не пробрасывает ошибку наружу (всё в Reason) и не
// возвращает nil.
//
// Режимы (s.cfg.Suggest.UrlscanMode):
//   - "off"/пусто — выключено, сеть не трогаем;
//   - "key"       — с заголовком API-Key (нужен external.urlscan_api_key);
//   - "anonymous" — без ключа.
//
// Пакетное состояние (кэш и пауза после 429) живёт в переменных с
// мьютексом: добавлять поля в Syncer нельзя.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DomainSuggestion — кандидат домена, найденный в сканах urlscan.
type DomainSuggestion struct {
	Domain     string `json:"domain"`
	Apex       string `json:"apex"`
	Scans      int    `json:"scans"`
	Requests   int    `json:"requests"`
	Confidence string `json:"confidence"` // high|medium|low
}

// DomainExcluded — домен, отсеянный фильтром (сам запрос или denylist).
type DomainExcluded struct {
	Domain string `json:"domain"`
	Reason string `json:"reason"`
}

// DomainSuggestions — итог совета: кандидаты плюс диагностика.
type DomainSuggestions struct {
	Enabled         bool               `json:"enabled"`
	Reason          string             `json:"reason"`
	Source          string             `json:"source"`
	Query           string             `json:"query"`
	Mode            string             `json:"mode"`
	ScansConsidered int                `json:"scans_considered"`
	Cached          bool               `json:"cached"`
	FetchedAt       time.Time          `json:"fetched_at"`
	Candidates      []DomainSuggestion `json:"candidates,omitempty"`
	Excluded        []DomainExcluded   `json:"excluded,omitempty"`
}

const (
	// suggestScansWindow — окно поиска сканов.
	suggestScansWindow = "date:>now-30d"
	// suggestMaxSearch — cap размера выдачи поиска (size=min(maxScans,10)).
	suggestMaxSearch = 10
	// suggestMaxResults — сколько UUID сканов готовы разобрать.
	suggestMaxResults = 5
	// suggestTimeout — таймаут HTTP-клиента.
	suggestTimeout = 15 * time.Second
	// suggestCooldown — пауза после 429, чтобы не долбить API.
	suggestCooldown = 5 * time.Minute
	// suggestTTL / suggestTTLAnonymous — TTL кэша: anonymous живёт дольше
	// (лимиты без ключа жёстче, повторные запросы дороже).
	suggestTTL          = 6 * time.Hour
	suggestTTLAnonymous = 24 * time.Hour
	// suggestMaxBody — ограничение тела ответа (защита от раздувания).
	suggestMaxBody int64 = 8 << 20
)

// urlscanBaseURL — шов для тестов (тесты подменяют на httptest).
var urlscanBaseURL = "https://urlscan.io"

// suggestHTTPClient — минимальный stdlib-клиент, без новых зависимостей.
var suggestHTTPClient = &http.Client{Timeout: suggestTimeout}

// suggestTrackerDenylist — встроенный отсев трекеров/рекламы: они видны в
// большинстве сканов и не несут информации о доменах самого сервиса.
// Совпадение точное или по суффиксу (www.google-analytics.com и т.п.).
var suggestTrackerDenylist = []string{
	"doubleclick.net",
	"google-analytics.com",
	"googletagmanager.com",
	"facebook.net",
	"hotjar.com",
	"criteo.com",
}

// Пакетное состояние: кэш результатов и пауза rate-limit (полей в Syncer
// добавлять нельзя — см. заголовок файла).
var (
	suggestStateMu   sync.Mutex
	suggestCache     = map[string]suggestCacheEntry{}
	suggestCoolUntil time.Time
)

type suggestCacheEntry struct {
	at  time.Time
	res *DomainSuggestions
}

// suggestAgg — агрегат по одному домену за все разобранные сканы.
type suggestAgg struct {
	scans    int  // в скольких разных сканах встречался
	requests int  // суммарное число вхождений в lists.domains
	umbrella bool // домен страницы скана в топе Umbrella (опц. поле)
}

// urlscanResultDoc — разбор ответа GET /api/v1/result/{uuid}/.
type urlscanResultDoc struct {
	Page struct {
		Domain string `json:"domain"`
	} `json:"page"`
	Lists struct {
		Domains []string `json:"domains"`
	} `json:"lists"`
	// Umbrella — опциональное поле: rank в топе Umbrella. Если API его не
	// отдаёт, ветка medium-уверенности просто не срабатывает.
	Umbrella *struct {
		Rank int `json:"rank"`
	} `json:"umbrella"`
}

var (
	errURLScanRateLimited = errors.New("urlscan: rate limited")
	errURLScanNotFound    = errors.New("urlscan: not found")
)

// SuggestDomains ищет домены сервиса по сканам urlscan.io: поиск
// `domain:<name> date:>now-30d` (size=min(maxScans,10)), затем result по
// ≤5 UUID, агрегация lists.domains (scans — в скольких сканах встретился
// домен, requests — сколько вхождений). Фильтр: apex самого запроса
// (поддомены оставляем) и denylist трекеров.
//
// Совет без побочных эффектов: nil не возвращается никогда, ошибки сети,
// лимитов и конфигурации уходят в Reason (Enabled=false).
func (s *Syncer) SuggestDomains(ctx context.Context, name string, maxScans int) *DomainSuggestions {
	mode, sources, apiKey := s.suggestSettings()
	res := &DomainSuggestions{Source: "urlscan", Mode: mode}

	switch {
	case mode != "key" && mode != "anonymous":
		res.Reason = fmt.Sprintf("подсказки выключены (suggest.urlscan_mode=%q)", mode)
		return res
	case mode == "key" && strings.TrimSpace(apiKey) == "":
		res.Reason = "режим key требует external.urlscan_api_key"
		return res
	case !suggestSourceEnabled(sources):
		res.Reason = "urlscan не включён в suggest.sources"
		return res
	}
	if normalizeSuggestDomain(name) == "" {
		res.Reason = "пустое имя домена"
		return res
	}
	name = normalizeSuggestDomain(name)

	size := maxScans
	if size <= 0 {
		size = suggestMaxSearch
	}
	if size > suggestMaxSearch {
		size = suggestMaxSearch
	}
	res.Query = "domain:" + name + " " + suggestScansWindow

	// Кэш: ключ — нормализованный запрос (плюс size — он влияет на выдачу).
	ttl := suggestCacheTTL(mode)
	cacheKey := res.Query + "|size=" + strconv.Itoa(size)
	if ent, ok := suggestCacheGet(cacheKey, ttl); ok {
		out := *ent
		out.Candidates = append([]DomainSuggestion(nil), ent.Candidates...)
		out.Excluded = append([]DomainExcluded(nil), ent.Excluded...)
		out.Cached = true
		return &out
	}

	// Пауза после 429: HTTP не трогаем (кэш проверили выше).
	if suggestCoolActive() {
		res.Reason = "rate limited"
		return res
	}
	if ctx.Err() != nil {
		res.Reason = "запрос прерван"
		return res
	}

	// Заголовок API-Key только в key-режиме; anonymous ходит без ключа.
	hdrKey := ""
	if mode == "key" {
		hdrKey = strings.TrimSpace(apiKey)
	}

	ids, err := urlscanSearch(ctx, res.Query, size, hdrKey)
	if err != nil {
		return suggestFail(res, ctx, err)
	}
	if len(ids) == 0 {
		res.Enabled = true
		res.FetchedAt = time.Now()
		res.Reason = "нет сканов за 30 дней"
		suggestCachePut(cacheKey, res)
		return res
	}
	if len(ids) > suggestMaxResults {
		ids = ids[:suggestMaxResults]
	}

	agg := map[string]*suggestAgg{}
	considered := 0
	var broken []string
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		doc, derr := urlscanResult(ctx, id, hdrKey)
		switch {
		case errors.Is(derr, errURLScanRateLimited):
			suggestMarkRateLimited()
			res.Reason = "rate limited"
			return res
		case errors.Is(derr, errURLScanNotFound):
			continue // 404: скан удалён — пропускаем UUID
		case derr != nil:
			broken = append(broken, id+": "+derr.Error())
			continue
		}
		considered++
		suggestAggregate(agg, doc)
	}
	res.ScansConsidered = considered
	if considered == 0 {
		switch {
		case ctx.Err() != nil:
			res.Reason = "запрос прерван"
			return res
		case len(broken) > 0:
			res.Reason = "не удалось разобрать сканы: " + strings.Join(broken, "; ")
			return res
		}
	}

	input, apex := name, suggestApex(name)
	domains := make([]string, 0, len(agg))
	for d := range agg {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	for _, d := range domains {
		a := agg[d]
		switch {
		case suggestIsTracker(d):
			res.Excluded = append(res.Excluded, DomainExcluded{
				Domain: d, Reason: "трекер/реклама (denylist)",
			})
		case d == input || d == apex:
			res.Excluded = append(res.Excluded, DomainExcluded{
				Domain: d, Reason: "сам запрошенный домен (apex)",
			})
		default:
			res.Candidates = append(res.Candidates, DomainSuggestion{
				Domain:     d,
				Apex:       suggestApex(d),
				Scans:      a.scans,
				Requests:   a.requests,
				Confidence: suggestConfidence(a.scans, a.umbrella),
			})
		}
	}
	sort.Slice(res.Candidates, func(i, j int) bool {
		if res.Candidates[i].Scans != res.Candidates[j].Scans {
			return res.Candidates[i].Scans > res.Candidates[j].Scans
		}
		if res.Candidates[i].Requests != res.Candidates[j].Requests {
			return res.Candidates[i].Requests > res.Candidates[j].Requests
		}
		return res.Candidates[i].Domain < res.Candidates[j].Domain
	})

	res.Enabled = true
	res.FetchedAt = time.Now()
	res.Reason = fmt.Sprintf("сканов: %d, кандидатов: %d", considered, len(res.Candidates))
	if len(broken) > 0 {
		res.Reason += fmt.Sprintf(" (не разобраны сканы: %d)", len(broken))
	}
	suggestCachePut(cacheKey, res)
	return res
}

// suggestFail — перевод ошибки запроса в Reason (Enabled=false, nil никогда).
func suggestFail(res *DomainSuggestions, ctx context.Context, err error) *DomainSuggestions {
	switch {
	case errors.Is(err, errURLScanRateLimited):
		suggestMarkRateLimited()
		res.Reason = "rate limited"
	case ctx.Err() != nil:
		res.Reason = "запрос прерван"
	default:
		res.Reason = "поиск не удался: " + err.Error()
	}
	return res
}

// suggestSettings — короткое чтение конфига под RLock (правило Config:
// блокировку держат только точки входа, вложенные хелперы — нет).
func (s *Syncer) suggestSettings() (mode string, sources []string, apiKey string) {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	return s.cfg.Suggest.UrlscanMode,
		append([]string(nil), s.cfg.Suggest.Sources...),
		s.cfg.External.UrlscanAPIKey
}

// suggestSourceEnabled — пустой списка = работаем в одиночку;
// непустой обязан содержать urlscan (preset/manual к сети не ходят).
func suggestSourceEnabled(sources []string) bool {
	if len(sources) == 0 {
		return true
	}
	for _, src := range sources {
		if strings.EqualFold(strings.TrimSpace(src), "urlscan") {
			return true
		}
	}
	return false
}

// suggestAggregate — подсчёт по одному скану: requests — каждое вхождение
// домена в lists.domains, scans — первое вхождение (дедуп внутри скана).
func suggestAggregate(agg map[string]*suggestAgg, doc *urlscanResultDoc) {
	seen := make(map[string]bool, len(doc.Lists.Domains))
	for _, raw := range doc.Lists.Domains {
		d := normalizeSuggestDomain(raw)
		if d == "" {
			continue
		}
		a := agg[d]
		if a == nil {
			a = &suggestAgg{}
			agg[d] = a
		}
		a.requests++
		if !seen[d] {
			seen[d] = true
			a.scans++
		}
	}
	// Опциональное поле: домен страницы скана в топе Umbrella. Если поля
	// нет — ветка не срабатывает, confidence считается только по scans.
	if doc.Umbrella != nil && doc.Umbrella.Rank > 0 {
		if pd := normalizeSuggestDomain(doc.Page.Domain); pd != "" {
			if a := agg[pd]; a != nil {
				a.umbrella = true
			}
		}
	}
}

// suggestConfidence — scans>=2 → high, иначе umbrella-топ → medium, low.
func suggestConfidence(scans int, umbrella bool) string {
	switch {
	case scans >= 2:
		return "high"
	case umbrella:
		return "medium"
	default:
		return "low"
	}
}

// urlscanSearch — GET /api/v1/search/?q=...&size=..., UUID из _id.
func urlscanSearch(ctx context.Context, query string, size int, hdrKey string) ([]string, error) {
	u := urlscanBaseURL + "/api/v1/search/?q=" +
		url.QueryEscape(query) + "&size=" + strconv.Itoa(size)
	var out struct {
		Results []struct {
			ID string `json:"_id"`
		} `json:"results"`
	}
	if err := urlscanGetJSON(ctx, u, hdrKey, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Results))
	for _, r := range out.Results {
		if id := strings.TrimSpace(r.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// urlscanResult — GET /api/v1/result/{uuid}/ (404 → errURLScanNotFound).
func urlscanResult(ctx context.Context, id, hdrKey string) (*urlscanResultDoc, error) {
	var doc urlscanResultDoc
	u := urlscanBaseURL + "/api/v1/result/" + url.PathEscape(id) + "/"
	if err := urlscanGetJSON(ctx, u, hdrKey, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// urlscanGetJSON — один GET с JSON-декодом; 429/404 — сантинелы выше.
func urlscanGetJSON(ctx context.Context, urlStr, hdrKey string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	if hdrKey != "" {
		req.Header.Set("API-Key", hdrKey)
	}
	resp, err := suggestHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return errURLScanRateLimited
	case http.StatusNotFound:
		return errURLScanNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("urlscan: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, suggestMaxBody)).Decode(out); err != nil {
		return fmt.Errorf("urlscan: decode: %w", err)
	}
	return nil
}

// suggestCacheTTL — TTL по режиму: anonymous дольше (6ч против 24ч).
func suggestCacheTTL(mode string) time.Duration {
	if mode == "anonymous" {
		return suggestTTLAnonymous
	}
	return suggestTTL
}

// suggestCacheGet — чтение кэша (протухшие записи удаляются).
func suggestCacheGet(key string, ttl time.Duration) (*DomainSuggestions, bool) {
	suggestStateMu.Lock()
	defer suggestStateMu.Unlock()
	ent, ok := suggestCache[key]
	if !ok {
		return nil, false
	}
	if time.Since(ent.at) > ttl {
		delete(suggestCache, key)
		return nil, false
	}
	return ent.res, true
}

// suggestCachePut — запись копии результата + ленивая вытравка протухшего
// (TTL проверяется при чтении, см. suggestCacheGet).
func suggestCachePut(key string, res *DomainSuggestions) {
	now := time.Now()
	clone := *res
	clone.Candidates = append([]DomainSuggestion(nil), res.Candidates...)
	clone.Excluded = append([]DomainExcluded(nil), res.Excluded...)

	suggestStateMu.Lock()
	defer suggestStateMu.Unlock()
	for k, ent := range suggestCache {
		// Максимальный TTL в пакете — suggestTTLAnonymous.
		if now.Sub(ent.at) > suggestTTLAnonymous {
			delete(suggestCache, k)
		}
	}
	suggestCache[key] = suggestCacheEntry{at: now, res: &clone}
}

// suggestCoolActive — активна ли пауза после 429.
func suggestCoolActive() bool {
	suggestStateMu.Lock()
	defer suggestStateMu.Unlock()
	return time.Now().Before(suggestCoolUntil)
}

// suggestMarkRateLimited — запомнить «до-когда не долбить» после 429.
func suggestMarkRateLimited() {
	suggestStateMu.Lock()
	defer suggestStateMu.Unlock()
	suggestCoolUntil = time.Now().Add(suggestCooldown)
}

// normalizeSuggestDomain — нижний регистр, без хвостовой точки.
func normalizeSuggestDomain(d string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
}

// suggestApex — грубый apex (последние две метки): точный eTLD+1 требует
// PSL, для отсева и подсказок достаточно (co.uk ловится как uk.co-пара —
// приемлемо: правила фильтра от этого не ломаются).
func suggestApex(d string) string {
	d = normalizeSuggestDomain(d)
	if i := strings.LastIndex(d, "."); i > 0 {
		return d[strings.LastIndex(d[:i], ".")+1:]
	}
	return d
}

// suggestIsTracker — домен из встроенного denylist трекеров.
func suggestIsTracker(d string) bool {
	for _, t := range suggestTrackerDenylist {
		if d == t || strings.HasSuffix(d, "."+t) {
			return true
		}
	}
	return false
}
