package storage

import (
	"context"
	"fmt"
	"time"
)

// Здесь всё, что нужно команде `music enrich`, помимо самого пайплайна
// (internal/enrich): список исполнителей для обхода и журнал прогонов
// enrich_runs — по тому же принципу, что и import_runs в import.go.

// ---------------------------------------------------------------------------
// Список исполнителей
// ---------------------------------------------------------------------------

// Artist — минимум, который нужен пайплайну обогащения про исполнителя.
type Artist struct {
	ID      int64
	NameRaw string // как показывать человеку и передавать провайдеру
	NameKey string // готовый стабильный ключ для кэша (см. cacheKey у EnrichArtist)
}

// ListArtists возвращает исполнителей, у которых есть хотя бы один
// присутствующий сейчас трек.
//
// "Присутствующий" — потому что смысла спрашивать Last.fm про исполнителя,
// чьи треки пропали из всех библиотек (is_present = false везде), нет:
// обогащать нечего, а квоту запросов к провайдеру потратим впустую.
// DISTINCT нужен, потому что у одного исполнителя обычно много треков,
// а строка со списком нужна ровно одна на исполнителя.
func (db *DB) ListArtists(ctx context.Context) ([]Artist, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT DISTINCT a.id, a.name_raw, a.name_key
		  FROM artists a
		  JOIN tracks t ON t.artist_id = a.id
		 WHERE t.is_present
		 ORDER BY a.id
	`)
	if err != nil {
		return nil, fmt.Errorf("список исполнителей для обогащения: %w", err)
	}
	defer rows.Close()

	var out []Artist
	for rows.Next() {
		var a Artist
		if err := rows.Scan(&a.ID, &a.NameRaw, &a.NameKey); err != nil {
			return nil, fmt.Errorf("чтение строки исполнителя: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Журнал прогонов обогащения
// ---------------------------------------------------------------------------

// EnrichRunStats — счётчики одного прогона, которые видно уже только
// по его завершении (в отличие от import_runs, тут нет одной транзакции
// на весь прогон — обогащение растянуто по времени из-за пауз между
// сетевыми запросами, см. комментарий у cmd/music main.go про enrich).
type EnrichRunStats struct {
	ArtistsTotal    int
	ArtistsEnriched int
	ArtistsFailed   int
	APICalls        int // сколько раз реально сходили в сеть (не считая кэш)
	CacheHits       int // сколько раз обошлись кэшем — по этой цифре видно, работает ли кэш вообще
}

// StartEnrichRun открывает запись в журнале прогонов обогащения.
//
// Возвращает id — его нужно передать в FinishEnrichRun, когда прогон
// закончится (успешно или с ошибкой).
func (db *DB) StartEnrichRun(ctx context.Context) (int64, error) {
	var id int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO enrich_runs (status)
		VALUES ('running')
		RETURNING id
	`).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("создание записи журнала обогащения: %w", err)
	}
	return id, nil
}

// FinishEnrichRun закрывает запись в журнале: проставляет счётчики,
// статус и время окончания.
//
// errText — текст ошибки, если прогон прервался не своей волей (например,
// провайдер лёг или пропал доступ — Р-017). Пустая строка для успешного
// завершения. status при этом должен быть "ok" или "failed" — так же,
// как в import_runs (CHECK-ограничение в схеме следит за этим на уровне базы).
func (db *DB) FinishEnrichRun(ctx context.Context, runID int64, status string, stats EnrichRunStats, errText string) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE enrich_runs
		   SET status           = $2,
		       artists_total    = $3,
		       artists_enriched = $4,
		       artists_failed   = $5,
		       api_calls        = $6,
		       cache_hits       = $7,
		       error            = $8,
		       finished_at      = now()
		 WHERE id = $1
	`, runID, status,
		stats.ArtistsTotal, stats.ArtistsEnriched, stats.ArtistsFailed,
		stats.APICalls, stats.CacheHits, errText)
	if err != nil {
		return fmt.Errorf("закрытие записи журнала обогащения (id=%d): %w", runID, err)
	}
	return nil
}

// EnrichRun — строка журнала обогащений, для команды `music enrich list`.
type EnrichRun struct {
	ID              int64
	Status          string
	ArtistsTotal    int
	ArtistsEnriched int
	ArtistsFailed   int
	APICalls        int
	CacheHits       int
	ErrorText       string
	StartedAt       time.Time
	FinishedAt      *time.Time
}

// EnrichRuns возвращает последние прогоны обогащения, самые свежие первыми.
func (db *DB) EnrichRuns(ctx context.Context, limit int) ([]EnrichRun, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, status, artists_total, artists_enriched, artists_failed,
		       api_calls, cache_hits, error, started_at, finished_at
		  FROM enrich_runs
		 ORDER BY started_at DESC
		 LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("запрос журнала обогащений: %w", err)
	}
	defer rows.Close()

	var out []EnrichRun
	for rows.Next() {
		var r EnrichRun
		if err := rows.Scan(
			&r.ID, &r.Status, &r.ArtistsTotal, &r.ArtistsEnriched, &r.ArtistsFailed,
			&r.APICalls, &r.CacheHits, &r.ErrorText, &r.StartedAt, &r.FinishedAt,
		); err != nil {
			return nil, fmt.Errorf("чтение строки журнала обогащений: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
