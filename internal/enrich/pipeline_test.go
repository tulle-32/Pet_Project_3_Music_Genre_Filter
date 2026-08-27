package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Фальшивые реализации — ни одна не ходит в сеть и не открывает базу.
// ---------------------------------------------------------------------------

// fakeProvider — GenreProvider, который сам решает, что "отдать по сети".
// Формат raw здесь — обычный JSON от []Tag: реальному Last.fm он совпадать
// не обязан, важно только, что TopTags и ParseTopTags согласованы между
// собой, ровно как того требует контракт интерфейса.
type fakeProvider struct {
	name      string
	tags      []Tag
	err       error
	callCount int // сколько раз реально вызвали TopTags (не ParseTopTags)
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) TopTags(_ context.Context, _ string) ([]Tag, []byte, error) {
	p.callCount++
	if p.err != nil {
		return nil, []byte("boom"), p.err
	}
	raw, _ := json.Marshal(p.tags)
	return p.tags, raw, nil
}

func (p *fakeProvider) ParseTopTags(raw []byte) ([]Tag, error) {
	var tags []Tag
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// fakeCache — provider_cache в виде map. Свежесть решает сам тест, выставляя
// поле fresh явно — пайплайну всё равно, ПОЧЕМУ запись свежая или нет,
// он просто следует этому флагу (как и в бою будет следовать тому, что
// скажет *storage.DB на основе CacheTTL).
type fakeCacheEntry struct {
	raw    []byte
	status string
	fresh  bool
}

type fakeCache struct {
	entries map[string]fakeCacheEntry
	puts    []fakeCacheEntry
}

func newFakeCache() *fakeCache {
	return &fakeCache{entries: make(map[string]fakeCacheEntry)}
}

// ttl принимается, но игнорируется: в тестах свежесть выставляет сам тест
// через поле fresh у fakeCacheEntry, а не реальное сравнение времени —
// параметр здесь только для соответствия интерфейсу CacheStore, у которого
// в бою (internal/storage.DB) ttl используется по-настоящему.
func (c *fakeCache) GetCache(_ context.Context, provider, requestKey string, _ time.Duration) ([]byte, string, bool, error) {
	e, ok := c.entries[provider+"|"+requestKey]
	if !ok {
		return nil, "", false, nil
	}
	return e.raw, e.status, e.fresh, nil
}

func (c *fakeCache) PutCache(_ context.Context, provider, requestKey string, raw []byte, status string) error {
	e := fakeCacheEntry{raw: raw, status: status}
	c.entries[provider+"|"+requestKey] = e
	c.puts = append(c.puts, e)
	return nil
}

// fakeWriter — artist_genres в виде среза записанных вызовов.
type fakeWriterCall struct {
	artistID   int64
	genreID    int32
	provider   string
	confidence float64
}

type fakeWriter struct {
	calls []fakeWriterCall
}

func (w *fakeWriter) UpsertArtistGenre(_ context.Context, artistID int64, genreID int32, provider string, confidence float64) error {
	w.calls = append(w.calls, fakeWriterCall{artistID, genreID, provider, confidence})
	return nil
}

// fakeResolver — справочник genre_aliases в виде map.
type fakeResolver map[string]int32

func (r fakeResolver) ResolveAlias(_ context.Context, aliasKey string) (int32, bool, error) {
	id, ok := r[aliasKey]
	return id, ok, nil
}

// ---------------------------------------------------------------------------
// Тесты
// ---------------------------------------------------------------------------

// Тестовый провайдер называется "lastfm" не просто так: он должен иметь вес
// в taxonomy.ProviderWeight, иначе Resolve откажется работать (см.
// TestResolve_UnknownProvider в internal/taxonomy) — "lastfm" там уже есть.

func TestEnrichArtist_CacheMiss_CallsProviderAndWritesCache(t *testing.T) {
	provider := &fakeProvider{name: "lastfm", tags: []Tag{{Name: "rock", Weight: 100}}}
	cache := newFakeCache()
	writer := &fakeWriter{}
	resolver := fakeResolver{"rock": 1}

	res, err := EnrichArtist(context.Background(), provider, cache, resolver, writer, 42, "Nirvana", "nirvana")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if provider.callCount != 1 {
		t.Errorf("ожидали один вызов TopTags, получили %d", provider.callCount)
	}
	if res.FromCache {
		t.Error("при промахе кэша FromCache должен быть false")
	}
	if res.MatchedGenres != 1 {
		t.Fatalf("ожидали 1 совпадение, получили %d", res.MatchedGenres)
	}
	if len(writer.calls) != 1 || writer.calls[0].artistID != 42 || writer.calls[0].genreID != 1 {
		t.Errorf("запись в artist_genres неверна: %+v", writer.calls)
	}
	if len(cache.puts) != 1 || cache.puts[0].status != "ok" {
		t.Errorf("ожидали одну запись в кэш со статусом ok: %+v", cache.puts)
	}
}

func TestEnrichArtist_FreshCache_DoesNotCallProvider(t *testing.T) {
	provider := &fakeProvider{name: "lastfm", tags: []Tag{{Name: "rock", Weight: 100}}}
	cache := newFakeCache()
	writer := &fakeWriter{}
	resolver := fakeResolver{"rock": 1}

	// Кладём в кэш заранее, как будто это уже было сделано в прошлом прогоне.
	raw, _ := json.Marshal(provider.tags)
	cache.entries["lastfm|lastfm:artist.gettoptags:nirvana"] = fakeCacheEntry{raw: raw, status: "ok", fresh: true}

	res, err := EnrichArtist(context.Background(), provider, cache, resolver, writer, 42, "Nirvana", "nirvana")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if provider.callCount != 0 {
		t.Errorf("TopTags не должен был вызываться при свежем кэше, вызван %d раз", provider.callCount)
	}
	if !res.FromCache {
		t.Error("FromCache должен быть true")
	}
	if res.MatchedGenres != 1 {
		t.Fatalf("ожидали 1 совпадение из кэша, получили %d", res.MatchedGenres)
	}
}

func TestEnrichArtist_FreshNotFound_SkipsEverything(t *testing.T) {
	provider := &fakeProvider{name: "lastfm"}
	cache := newFakeCache()
	writer := &fakeWriter{}
	resolver := fakeResolver{}

	cache.entries["lastfm|lastfm:artist.gettoptags:unknown-artist"] = fakeCacheEntry{status: "not_found", fresh: true}

	res, err := EnrichArtist(context.Background(), provider, cache, resolver, writer, 99, "Unknown Artist", "unknown-artist")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if provider.callCount != 0 {
		t.Error("TopTags не должен был вызываться")
	}
	if len(writer.calls) != 0 {
		t.Errorf("writer не должен был вызываться, получили: %+v", writer.calls)
	}
	if res.MatchedGenres != 0 {
		t.Errorf("ожидали 0 совпадений, получили %d", res.MatchedGenres)
	}
}

// TestEnrichArtist_ProviderError_CachesErrorButPropagatesIt — ключевой
// случай из Р-017: ошибка (например, ErrAccessDenied) кэшируется статусом
// "error" ДЛЯ ИСТОРИИ, но сама EnrichArtist обязана вернуть эту ошибку
// вызывающему коду, а не проглотить её молча.
func TestEnrichArtist_ProviderError_CachesErrorButPropagatesIt(t *testing.T) {
	boom := errors.New("lastfm: доступ запрещён")
	provider := &fakeProvider{name: "lastfm", err: boom}
	cache := newFakeCache()
	writer := &fakeWriter{}
	resolver := fakeResolver{}

	_, err := EnrichArtist(context.Background(), provider, cache, resolver, writer, 1, "Nirvana", "nirvana")
	if !errors.Is(err, boom) {
		t.Fatalf("ожидали, что ошибка провайдера дойдёт до вызывающего кода, получили: %v", err)
	}
	if len(writer.calls) != 0 {
		t.Errorf("writer не должен был вызываться при ошибке провайдера: %+v", writer.calls)
	}
	if len(cache.puts) != 1 || cache.puts[0].status != "error" {
		t.Errorf("ожидали запись в кэш со статусом error: %+v", cache.puts)
	}
}

func TestEnrichArtist_UnmappedTagsReturned(t *testing.T) {
	provider := &fakeProvider{name: "lastfm", tags: []Tag{{Name: "seen live", Weight: 50}}}
	cache := newFakeCache()
	writer := &fakeWriter{}
	resolver := fakeResolver{} // справочник пуст — тег не найдётся

	res, err := EnrichArtist(context.Background(), provider, cache, resolver, writer, 1, "Nirvana", "nirvana")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(res.Unmapped) != 1 || res.Unmapped[0] != "seen live" {
		t.Errorf("ожидали unmapped=[\"seen live\"], получили: %+v", res.Unmapped)
	}
	if res.MatchedGenres != 0 {
		t.Errorf("ожидали 0 совпадений, получили %d", res.MatchedGenres)
	}
}

// Небольшая проверка на всякий случай: если бы кто-то забыл добавить вес
// провайдера в taxonomy.ProviderWeight, EnrichArtist должна честно упасть,
// а не молча посчитать нулевую уверенность.
func TestEnrichArtist_UnknownProviderWeight(t *testing.T) {
	provider := &fakeProvider{name: "unknown-provider", tags: []Tag{{Name: "rock", Weight: 100}}}
	cache := newFakeCache()
	writer := &fakeWriter{}
	resolver := fakeResolver{"rock": 1}

	_, err := EnrichArtist(context.Background(), provider, cache, resolver, writer, 1, "Nirvana", "nirvana")
	if err == nil {
		t.Fatal("ожидали ошибку про неизвестный вес провайдера")
	}
}
