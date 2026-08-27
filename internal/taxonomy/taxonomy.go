// Пакет taxonomy сводит сырые теги провайдера к нашему справочнику жанров
// и считает уверенность — ровно то место в пайплайне, которое в
// docs/ARCHITECTURE.md называется "справочник тег → жанр" плюс "расчёт
// уверенности" (раздел "Пайплайн определения жанра").
//
// Пакет ничего не знает ни про HTTP, ни про конкретного провайдера
// (Last.fm, MusicBrainz, Deezer) — только про две вещи: как искать жанр
// по тегу (интерфейс AliasResolver, за которым в бою стоит
// internal/storage) и какой вес у какого провайдера в общей уверенности.
//
// Важная деталь устройства пакетов: taxonomy НЕ импортирует internal/enrich,
// хотя по смыслу теги оттуда и приходят. Причина чисто техническая: план
// в docs/ARCHITECTURE.md кладёт pipeline.go (кэш → провайдеры → теги →
// уверенность) внутрь самого internal/enrich, а этому файлу нужно вызывать
// taxonomy.Resolve. Если бы taxonomy при этом импортировал enrich ради
// типа Tag, а enrich импортировал taxonomy ради Resolve — получился бы
// цикл импортов, который Go просто не соберёт. Поэтому у taxonomy свой
// маленький Tag, по форме идентичный enrich.Tag, а пайплайн внутри
// internal/enrich делает тривиальное преобразование одного в другой.
// Мелкое дублирование ради разрыва цикла — обычная и дешёвая плата.
package taxonomy

import (
	"context"
	"fmt"
	"strings"
)

// Tag — тег провайдера в том виде, в каком его видит taxonomy: имя и
// сила по шкале 0..100. Форма нарочно совпадает с enrich.Tag — это два
// независимых типа с одинаковыми полями, а не один и тот же тип под двумя
// именами (подробности — в комментарии к пакету выше).
type Tag struct {
	Name   string
	Weight int
}

// ProviderWeight — вес провайдера в формуле уверенности.
//
// Числа и сама формула — из docs/ARCHITECTURE.md, раздел "Как считается
// уверенность": musicbrainz 0.50, lastfm 0.35, deezer 0.15. Сейчас в карте
// только lastfm, потому что только он и реализован (v0.7.0); остальные
// добавятся вместе со своими клиентами в v0.8.0 и v1.3.0 — тогда же встанет
// вопрос, как именно объединять уверенность НЕСКОЛЬКИХ провайдеров для
// одной пары исполнитель-жанр (сумма из ARCHITECTURE.md — Σ по всем
// провайдерам, а artist_genres хранит их отдельными строками, см. PRIMARY
// KEY (artist_id, genre_id, provider) в 00001_initial_schema.sql). Это
// сознательно отложено: решать эту задачу без второго провайдера в руках
// означало бы гадать, а не проектировать.
var ProviderWeight = map[string]float64{
	"lastfm": 0.35,
	// "musicbrainz": 0.50, // v0.8.0
	// "deezer":      0.15, // v1.3.0
}

// AliasResolver — то немногое, что нужно от справочника жанров: по тегу
// вернуть id жанра, если он есть.
//
// Отдельный интерфейс, а не прямая зависимость от internal/storage —
// затем же, зачем и everywhere в этом проекте: Resolve можно проверить
// тестами без настоящей базы, подсунув фальшивый резолвер над обычной
// map (см. taxonomy_test.go). В бою эту роль играет *storage.DB.
type AliasResolver interface {
	// ResolveAlias ищет жанр по тегу. aliasKey должен быть уже приведён
	// к нижнему регистру и без пробелов по краям — ровно так же, как
	// ключи заливаются в genre_aliases командой "music seed genres"
	// (internal/storage/genres.go, SeedGenres). Если эти две нормализации
	// разойдутся, справочник перестанет находиться, поэтому нормализация
	// теговых имён и в Resolve, и там — намеренно один и тот же приём:
	// strings.ToLower + strings.TrimSpace, ни одной другой чистки.
	ResolveAlias(ctx context.Context, aliasKey string) (genreID int32, found bool, err error)
}

// Match — во что превратился один тег или группа тегов: жанр и уверенность
// от 0 до 1, готовая лечь в колонку artist_genres.confidence.
type Match struct {
	GenreID    int32
	Confidence float64
}

// Resolve сводит теги ОДНОГО провайдера к жанрам с уверенностью.
//
// Контракт со стороны провайдера (см. enrich.Tag): Weight — всегда шкала
// 0..100, независимо от того, кто его прислал. Last.fm отдаёт такую шкалу
// сам (это count тега). Когда появятся MusicBrainz и Deezer, их клиенты
// обязаны перевести СВОЙ сигнал на эту же шкалу до того, как отдать Tag
// наружу: например, MusicBrainz — 100 для полноценного genre и 60 для
// обычного тега, что при делении на 100 даёт ровно те 1.0 и 0.6 из
// формулы в docs/ARCHITECTURE.md. Taxonomy этой арифметики провайдеров
// не видит и не должна — только общую шкалу 0..100 и вес самого
// провайдера (см. ProviderWeight).
//
// Несколько тегов одного провайдера иногда указывают на один и тот же наш
// жанр — например, Last.fm может прислать и "punk", и "punk rock" сразу,
// а оба в data/genre_map.json ведут в один и тот же код punk_rock.
// Складывать их уверенность было бы неверно: это не два независимых
// мнения, а одно и то же мнение, просто выраженное двумя тегами. Поэтому
// на одинаковый жанр берётся МАКСИМУМ, а не сумма.
//
// Теги, для которых в справочнике нет alias, не считаются ошибкой —
// это ожидаемо (Last.fm легко может прислать "seen live" или
// "favourite"). Они возвращаются отдельным списком unmapped, из которого
// в будущем вырастет отчёт о нераспознанных тегах (data/genre_map.json,
// команда "music genres unmapped", ещё не реализована).
func Resolve(ctx context.Context, resolver AliasResolver, provider string, tags []Tag) (matches []Match, unmapped []string, err error) {
	weight, ok := ProviderWeight[provider]
	if !ok {
		return nil, nil, fmt.Errorf("taxonomy: неизвестный провайдер %q, вес в ProviderWeight не задан", provider)
	}

	best := make(map[int32]float64)

	for _, tag := range tags {
		key := strings.ToLower(strings.TrimSpace(tag.Name))
		if key == "" {
			continue
		}

		genreID, found, resolveErr := resolver.ResolveAlias(ctx, key)
		if resolveErr != nil {
			return nil, nil, fmt.Errorf("taxonomy: поиск жанра по тегу %q: %w", key, resolveErr)
		}
		if !found {
			unmapped = append(unmapped, key)
			continue
		}

		// clamp на случай сюрприза от провайдера (например, если Last.fm
		// когда-нибудь начнёт отдавать count больше 100 — такого не было,
		// но явная защита дешевле, чем уверенность больше единицы в базе).
		strength := clamp01(float64(tag.Weight) / 100.0)
		confidence := clamp01(weight * strength)

		if confidence > best[genreID] {
			best[genreID] = confidence
		}
	}

	for genreID, confidence := range best {
		matches = append(matches, Match{GenreID: genreID, Confidence: confidence})
	}
	return matches, unmapped, nil
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
