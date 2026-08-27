// Пайплайн обогащения одного исполнителя: кэш → провайдер → теги →
// уверенность → запись в базу. Ровно то, что нарисовано в
// docs/ARCHITECTURE.md, раздел "Пайплайн определения жанра".
//
// Файл нарочно не знает ни про Last.fm конкретно (только про интерфейс
// GenreProvider из provider.go этого же пакета), ни про SQL (только про
// маленькие интерфейсы CacheStore и GenreWriter ниже, за которыми в бою
// стоит *storage.DB). Это тот же приём, что и everywhere в проекте:
// pipeline можно проверить тестами на фальшивых реализациях, не поднимая
// настоящую базу и не стуча в настоящий Last.fm.
package enrich

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tulle-32/Pet_Project_3_Music_Genre_Filter/internal/taxonomy"
)

// CacheTTL — срок жизни записи в provider_cache, после которого пайплайн
// считает её устаревшей и спрашивает провайдера заново.
//
// Число — из docs/ARCHITECTURE.md, таблица "Пороги": 30 дней.
const CacheTTL = 30 * 24 * time.Hour

// CacheStore — то немногое, что пайплайну нужно от provider_cache.
type CacheStore interface {
	// GetCache возвращает сырой ответ, если он есть, и признак свежести.
	//
	// ВАЖНАЯ ДЕТАЛЬ: fresh обязан быть false для status == "error", даже
	// если запись моложе CacheTTL. Причина — Р-017 в docs/DECISIONS.md:
	// ошибка "нет доступа" (например, геоблокировка Last.fm без VPN) —
	// это состояние ОКРУЖЕНИЯ, а не свойство исполнителя. Если закэшировать
	// такую ошибку как "свежую" на 30 дней, пайплайн продолжит молча
	// пропускать всех исполнителей ещё месяц ПОСЛЕ того, как VPN снова
	// включили, — и это будет куда более странный баг, чем лишний повторный
	// запрос к провайдеру. "Не найдено" (артист неизвестен провайдеру) —
	// другое дело, это устойчивый факт, а не сбой среды, поэтому такой
	// статус кэшируется как обычно.
	GetCache(ctx context.Context, provider, requestKey string, ttl time.Duration) (raw []byte, status string, fresh bool, err error)

	// PutCache сохраняет сырой ответ (или факт ошибки) в кэш.
	PutCache(ctx context.Context, provider, requestKey string, raw []byte, status string) error
}

// GenreWriter — то немногое, что пайплайну нужно от artist_genres.
type GenreWriter interface {
	UpsertArtistGenre(ctx context.Context, artistID int64, genreID int32, provider string, confidence float64) error
}

// Result — что получилось по итогам обогащения одного исполнителя.
// Возвращается наружу (в cmd/music) для журнала и статистики прогона.
type Result struct {
	MatchedGenres int      // сколько жанров записано в artist_genres
	Unmapped      []string // теги, для которых в справочнике не нашлось жанра
	FromCache     bool     // ответ пришёл из кэша, в сеть не ходили
}

// EnrichArtist обогащает ОДНОГО исполнителя одним провайдером.
//
// artistName — уже normalize.PrimaryArtist(name_raw), см. комментарий
// у GenreProvider.TopTags в provider.go: пайплайн этим не занимается,
// это забота вызывающего кода (он ближе к данным исполнителя).
// cacheKey — стабильный ключ для этого исполнителя и этого провайдера;
// вызывающий код передаёт artists.name_key (он уже посчитан и хранится
// в базе для дедупликации — пересчитывать его здесь смысла нет).
func EnrichArtist(
	ctx context.Context,
	provider GenreProvider,
	cache CacheStore,
	resolver taxonomy.AliasResolver,
	writer GenreWriter,
	artistID int64,
	artistName string,
	cacheKey string,
) (Result, error) {
	requestKey := provider.Name() + ":artist.gettoptags:" + cacheKey

	tags, fromCache, err := fetchTags(ctx, provider, cache, requestKey, artistName)
	if err != nil {
		return Result{}, err
	}

	// Приводим enrich.Tag к taxonomy.Tag — тривиальное преобразование,
	// разрывающее цикл импортов между enrich и taxonomy (подробности в
	// комментарии к пакету taxonomy).
	ttags := make([]taxonomy.Tag, len(tags))
	for i, t := range tags {
		ttags[i] = taxonomy.Tag{Name: t.Name, Weight: t.Weight}
	}

	matches, unmapped, err := taxonomy.Resolve(ctx, resolver, provider.Name(), ttags)
	if err != nil {
		return Result{}, fmt.Errorf("обогащение исполнителя %q: %w", artistName, err)
	}

	for _, m := range matches {
		if err := writer.UpsertArtistGenre(ctx, artistID, m.GenreID, provider.Name(), m.Confidence); err != nil {
			return Result{}, fmt.Errorf("обогащение исполнителя %q: %w", artistName, err)
		}
	}

	return Result{
		MatchedGenres: len(matches),
		Unmapped:      unmapped,
		FromCache:     fromCache,
	}, nil
}

// fetchTags достаёт сырой ответ провайдера — из кэша, если он свежий,
// иначе через настоящий сетевой запрос, с записью результата (успеха
// или ошибки) обратно в кэш.
func fetchTags(
	ctx context.Context,
	provider GenreProvider,
	cache CacheStore,
	requestKey, artistName string,
) (tags []Tag, fromCache bool, err error) {
	raw, status, fresh, err := cache.GetCache(ctx, provider.Name(), requestKey, CacheTTL)
	if err != nil {
		return nil, false, fmt.Errorf("чтение кэша для %q: %w", requestKey, err)
	}

	if fresh {
		// status здесь может быть только "ok" или "not_found" — see
		// комментарий CacheStore.GetCache про то, почему "error" никогда
		// не бывает fresh. Для "not_found" провайдер уже когда-то сказал
		// "не знаю такого", raw пуст, и это законные ноль тегов.
		if status == "not_found" || len(raw) == 0 {
			return nil, true, nil
		}
		tags, err = provider.ParseTopTags(raw)
		if err != nil {
			return nil, false, fmt.Errorf("разбор кэшированного ответа для %q: %w", requestKey, err)
		}
		return tags, true, nil
	}

	tags, raw, providerErr := provider.TopTags(ctx, artistName)
	if providerErr != nil {
		// Сетевые и прочие ошибки (включая ErrAccessDenied из Р-017)
		// кэшируются статусом "error" — специально НЕ как fresh при
		// следующем чтении (см. GetCache), чтобы временная недоступность
		// провайдера не превратилась в месячную немоту всего обогащения.
		// Это только запись истории, а не подавление повторных попыток.
		// raw здесь может быть пустым (если запрос вообще не дошёл) —
		// это не страшно, PutCache просто сохранит пустое тело.
		_ = cache.PutCache(ctx, provider.Name(), requestKey, raw, "error")
		return nil, false, providerErr
	}

	status = "ok"
	if len(tags) == 0 {
		status = "not_found"
	}
	// В кэш идёт ИМЕННО raw — те же байты, что вернул сам провайдер, без
	// какой-либо нашей обработки. Ровно поэтому GenreProvider.ParseTopTags
	// умеет разобрать их что при свежем запросе, что позже из кэша: это
	// буквально один и тот же вход для одного и того же кода.
	if putErr := cache.PutCache(ctx, provider.Name(), requestKey, raw, status); putErr != nil {
		return nil, false, fmt.Errorf("запись кэша для %q: %w", requestKey, putErr)
	}

	return tags, false, nil
}

// unmappedPreview — короткая строка для журналов: первые несколько
// нераспознанных тегов через запятую, а не весь список, если он длинный.
func unmappedPreview(unmapped []string, limit int) string {
	if len(unmapped) == 0 {
		return ""
	}
	if len(unmapped) <= limit {
		return strings.Join(unmapped, ", ")
	}
	return strings.Join(unmapped[:limit], ", ") + fmt.Sprintf(" и ещё %d", len(unmapped)-limit)
}
