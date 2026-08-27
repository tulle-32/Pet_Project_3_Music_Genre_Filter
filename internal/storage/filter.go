package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Здесь — тот самый смысл всего проекта: отдать список треков одного жанра
// (и всех его поджанров) из конкретной библиотеки. Запрос почти дословно
// взят из docs/ARCHITECTURE.md, раздел "Логика фильтра" — тот SQL был
// написан заранее, ещё на этапе проектирования (v0.1.0), и всё это время
// ждал, когда до него дойдёт очередь.

// FilteredTrack — одна строка результата фильтра.
type FilteredTrack struct {
	Artist      string
	Title       string
	DurationSec int // 0, если источник не сообщил длительность
	Confidence  float64
}

// FilterTracks возвращает треки библиотеки, чей исполнитель отнесён к
// жанру genreCode или любому его поджанру, с уверенностью не ниже
// minConfidence.
//
// "Поджанр" считается через рекурсивный запрос: попросили "rock" — получили
// вместе с ним punk_rock, hard_rock, grunge и так далее, сколько бы уровней
// вложенности ни было в data/genre_map.json. Именно ради такого запроса
// (WITH RECURSIVE) в своё время выбрали PostgreSQL, а не SQLite (Р-002).
//
// minConfidence — порог из docs/ARCHITECTURE.md ("Пороги": по умолчанию
// 0.50). Передаётся параметром, а не зашит константой, потому что порог —
// это как раз то место, где хочется покрутить настройку самому, не трогая
// код: одному человеку 0.50 может быть мало доверия, другому — с запасом.
//
// Результат отсортирован по убыванию уверенности, а внутри одной
// уверенности — по имени исполнителя и названию трека: и человеку удобнее
// смотреть на список глазами (сначала то, в чём уверены больше), и порядок
// стабилен от запуска к запуску.
func (db *DB) FilterTracks(ctx context.Context, libraryTitle, genreCode string, minConfidence float64) ([]FilteredTrack, error) {
	var libraryID int64
	err := db.Pool.QueryRow(ctx,
		`SELECT id FROM libraries WHERE title = $1`, libraryTitle).Scan(&libraryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("библиотека %q не найдена (посмотреть точные названия: music library list)", libraryTitle)
		}
		return nil, fmt.Errorf("поиск библиотеки %q: %w", libraryTitle, err)
	}

	var genreExists bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM genres WHERE code = $1)`, genreCode,
	).Scan(&genreExists); err != nil {
		return nil, fmt.Errorf("проверка жанра %q: %w", genreCode, err)
	}
	if !genreExists {
		return nil, fmt.Errorf("жанр с кодом %q не найден (посмотреть дерево целиком: music genres tree)", genreCode)
	}

	rows, err := db.Pool.Query(ctx, `
WITH RECURSIVE genre_tree AS (
    -- якорь: сам жанр, который попросили
    SELECT id FROM genres WHERE code = $2
    UNION ALL
    -- шаг: все поджанры, поджанры поджанров и так далее
    SELECT g.id FROM genres g
    JOIN genre_tree gt ON g.parent_id = gt.id
)
SELECT a.name_raw, t.title_raw, t.duration_sec, ag.confidence
  FROM tracks t
  JOIN artists a        ON a.id = t.artist_id
  JOIN artist_genres ag ON ag.artist_id = a.id
 WHERE t.library_id  = $1
   AND ag.genre_id   IN (SELECT id FROM genre_tree)
   AND ag.confidence >= $3
   AND t.is_present
   -- ручная правка "это не тот жанр" перевешивает алгоритм, даже если
   -- сама правка появится позже (overrides пока не заполняется ни одной
   -- командой — таблица и это условие готовы заранее, на вырост, как и
   -- было решено в Р-006 про libraries).
   AND NOT EXISTS (
       SELECT 1 FROM overrides o
        WHERE o.action = 'remove'
          AND o.genre_id IN (SELECT id FROM genre_tree)
          AND ((o.scope = 'track'  AND o.target_id = t.id)
            OR (o.scope = 'artist' AND o.target_id = a.id))
   )
 ORDER BY ag.confidence DESC, a.name_key, t.title_key
	`, libraryID, genreCode, minConfidence)
	if err != nil {
		return nil, fmt.Errorf("запрос фильтра по жанру %q: %w", genreCode, err)
	}
	defer rows.Close()

	var out []FilteredTrack
	for rows.Next() {
		var (
			ft       FilteredTrack
			duration *int
		)
		if err := rows.Scan(&ft.Artist, &ft.Title, &duration, &ft.Confidence); err != nil {
			return nil, fmt.Errorf("чтение строки фильтра: %w", err)
		}
		if duration != nil {
			ft.DurationSec = *duration
		}
		out = append(out, ft)
	}
	return out, rows.Err()
}
