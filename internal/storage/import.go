package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tulle-32/Pet_Project_3_Music_Genre_Filter/internal/normalize"
	"github.com/tulle-32/Pet_Project_3_Music_Genre_Filter/internal/sources"
)

// Здесь живёт импорт: превращение сырого списка треков в записи базы.
//
// Порядок действий, если смотреть сверху:
//
//	1. найти или создать библиотеку
//	2. открыть запись в журнале импортов
//	3. схлопнуть дубли внутри самого файла
//	4. для каждого трека: найти или создать исполнителя, записать трек
//	5. пометить пропавшие треки как отсутствующие
//	6. закрыть запись в журнале
//
// Всё это делается в одной транзакции. Если что-то сломается на середине,
// база останется ровно в том состоянии, в каком была до вызова. Половины
// импорта не бывает.

// ImportResult — что получилось по итогам импорта.
type ImportResult struct {
	LibraryID  int64
	RunID      int64
	FromSource int // сколько строк было в источнике
	Duplicates int // сколько из них оказались повторами внутри файла
	Unique     int // сколько осталось после схлопывания
	NewTracks  int // сколько треков появилось в базе впервые
	NewArtists int // сколько исполнителей завелось впервые
	Gone       int // сколько треков пропало из библиотеки с прошлого раза
	Took       time.Duration
}

// ImportTracks заливает треки источника в библиотеку.
func (db *DB) ImportTracks(
	ctx context.Context,
	libraryTitle string,
	src sources.TrackSource,
	raw []sources.RawTrack,
) (*ImportResult, error) {

	started := time.Now()
	result := &ImportResult{FromSource: len(raw)}

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("начало транзакции: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// --- 1. Библиотека ------------------------------------------------------

	libraryID, err := findOrCreateLibrary(ctx, tx, libraryTitle, src)
	if err != nil {
		return nil, err
	}
	result.LibraryID = libraryID

	// --- 2. Запись в журнале ------------------------------------------------

	var runID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO import_runs (library_id, source_name, source_ref, status)
		VALUES ($1, $2, $3, 'running')
		RETURNING id
	`, libraryID, src.Name(), src.Ref()).Scan(&runID)
	if err != nil {
		return nil, fmt.Errorf("создание записи журнала импорта: %w", err)
	}
	result.RunID = runID

	// --- 3. Схлопывание дублей внутри файла ---------------------------------

	unique := dedupe(raw)
	result.Unique = len(unique)
	result.Duplicates = result.FromSource - result.Unique

	// --- 4. Запись треков ---------------------------------------------------

	// Копим id всех треков, которые встретились в этой выгрузке.
	// Понадобится на шаге 5, чтобы вычислить пропавшие.
	seenTrackIDs := make([]int64, 0, len(unique))

	// Кэш исполнителей на время импорта: ключ → id. Без него мы бы ходили
	// в базу за одним и тем же исполнителем столько раз, сколько у него
	// треков. На библиотеке в две тысячи треков это разница примерно
	// в полторы тысячи лишних запросов.
	artistCache := make(map[string]int64, len(unique)/2)

	for _, t := range unique {
		// Проверяем отмену на каждом треке: импорт большой выгрузки
		// длится ощутимо, и Ctrl+C должен работать сразу.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		artistID, isNewArtist, err := findOrCreateArtist(ctx, tx, artistCache, t.Artist)
		if err != nil {
			return nil, err
		}
		if isNewArtist {
			result.NewArtists++
		}

		trackID, isNewTrack, err := upsertTrack(ctx, tx, libraryID, artistID, t)
		if err != nil {
			return nil, err
		}
		if isNewTrack {
			result.NewTracks++
		}
		seenTrackIDs = append(seenTrackIDs, trackID)
	}

	// --- 5. Пропавшие треки -------------------------------------------------

	// Треки не удаляем. Человек мог убрать песню из ВК, а нам полезно
	// помнить, что она когда-то была: и для истории, и чтобы при возврате
	// не потерять проставленный жанр и ручные правки.
	//
	// $2 — массив идентификаторов. Запись "id <> ALL($2)" читается как
	// "id не совпадает ни с одним элементом массива".
	gone, err := tx.Exec(ctx, `
		UPDATE tracks
		   SET is_present = FALSE
		 WHERE library_id = $1
		   AND is_present
		   AND id <> ALL($2::bigint[])
	`, libraryID, seenTrackIDs)
	if err != nil {
		return nil, fmt.Errorf("пометка пропавших треков: %w", err)
	}
	result.Gone = int(gone.RowsAffected())

	// --- 6. Закрываем журнал ------------------------------------------------

	_, err = tx.Exec(ctx, `
		UPDATE import_runs
		   SET status = 'ok',
		       tracks_seen = $2,
		       tracks_new  = $3,
		       tracks_gone = $4,
		       finished_at = now()
		 WHERE id = $1
	`, runID, result.Unique, result.NewTracks, result.Gone)
	if err != nil {
		return nil, fmt.Errorf("закрытие записи журнала: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("фиксация транзакции: %w", err)
	}

	result.Took = time.Since(started)
	return result, nil
}

// ---------------------------------------------------------------------------
// Шаги по отдельности
// ---------------------------------------------------------------------------

// dedupe схлопывает повторы внутри одной выгрузки.
//
// В библиотеке ВК один и тот же трек часто лежит несколько раз: добавили
// с разных страниц, перезалили в лучшем качестве, случайно продублировали.
// Для нас это один трек, но с несколькими идентификаторами источника —
// поэтому идентификаторы собираем в кучу, а не выбрасываем.
func dedupe(raw []sources.RawTrack) []dedupedTrack {
	// Порядок важен: map в Go при переборе выдаёт элементы в случайном
	// порядке, а мы хотим стабильный результат от прогона к прогону.
	// Поэтому сам список храним срезом, а map используем только для
	// поиска "видели ли уже такой ключ".
	index := make(map[string]int, len(raw))
	out := make([]dedupedTrack, 0, len(raw))

	for _, t := range raw {
		artistKey := normalize.Artist(t.Artist)
		titleKey := normalize.Title(t.Title)

		// Треки без исполнителя или без названия пропускаем: сопоставить
		// такое с внешним справочником всё равно не выйдет.
		if artistKey == "" || titleKey == "" {
			continue
		}

		// \x00 — символ, которого не бывает в нормализованных строках.
		// Поэтому склейка через него однозначна: "ab"+"c" и "a"+"bc"
		// дадут разные ключи, а через обычный пробел дали бы одинаковые.
		key := artistKey + "\x00" + titleKey

		if i, seen := index[key]; seen {
			// Уже видели: добавляем идентификатор и берём длительность,
			// если раньше её не было.
			if t.SourceID != "" {
				out[i].SourceIDs = append(out[i].SourceIDs, t.SourceID)
			}
			if out[i].DurationSec == 0 && t.DurationSec > 0 {
				out[i].DurationSec = t.DurationSec
			}
			continue
		}

		d := dedupedTrack{
			Artist:      normalize.PrimaryArtist(t.Artist),
			ArtistKey:   artistKey,
			Title:       t.Title,
			TitleKey:    titleKey,
			DurationSec: t.DurationSec,
		}
		if t.SourceID != "" {
			d.SourceIDs = []string{t.SourceID}
		}

		index[key] = len(out)
		out = append(out, d)
	}
	return out
}

// dedupedTrack — трек после схлопывания: с готовыми ключами и списком
// идентификаторов из источника.
type dedupedTrack struct {
	Artist      string // исходное написание, для показа человеку
	ArtistKey   string // нормализованное, для сравнения
	Title       string
	TitleKey    string
	DurationSec int
	SourceIDs   []string
}

// findOrCreateLibrary находит библиотеку по названию или создаёт новую.
//
// Уникального ограничения на название в схеме нет — сейчас библиотеками
// управляет один человек через командную строку, и защищать тут нечего.
// Когда появятся пользователи (v2.0.0), понадобится уникальность в паре
// с владельцем, и это будет отдельная миграция.
func findOrCreateLibrary(
	ctx context.Context, tx pgx.Tx, title string, src sources.TrackSource,
) (int64, error) {

	var id int64
	err := tx.QueryRow(ctx,
		`SELECT id FROM libraries WHERE title = $1`, title).Scan(&id)

	// pgx.ErrNoRows означает "запрос отработал, но строк не нашлось".
	// Это не ошибка, а нормальный ответ — поэтому проверяем отдельно
	// через errors.Is, а не считаем любую ошибку провалом.
	if err == nil {
		// Библиотека уже есть: обновляем, откуда её в последний раз брали.
		_, err = tx.Exec(ctx, `
			UPDATE libraries
			   SET source_name = $2, source_ref = $3, updated_at = now()
			 WHERE id = $1
		`, id, src.Name(), src.Ref())
		if err != nil {
			return 0, fmt.Errorf("обновление библиотеки: %w", err)
		}
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("поиск библиотеки: %w", err)
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO libraries (title, source_name, source_ref)
		VALUES ($1, $2, $3)
		RETURNING id
	`, title, src.Name(), src.Ref()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("создание библиотеки: %w", err)
	}
	return id, nil
}

// findOrCreateArtist ищет исполнителя по нормализованному ключу,
// заглядывая заодно в таблицу псевдонимов.
func findOrCreateArtist(
	ctx context.Context, tx pgx.Tx, cache map[string]int64, rawName string,
) (int64, bool, error) {

	nameKey := normalize.Artist(rawName)

	if id, ok := cache[nameKey]; ok {
		return id, false, nil
	}

	var id int64

	// Сначала прямое совпадение по ключу.
	err := tx.QueryRow(ctx,
		`SELECT id FROM artists WHERE name_key = $1`, nameKey).Scan(&id)
	if err == nil {
		cache[nameKey] = id
		return id, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("поиск исполнителя %q: %w", nameKey, err)
	}

	// Потом по псевдонимам: "киш" должен найти "Король и Шут".
	// Пока эта таблица пустая, она наполнится в v0.8.0 из MusicBrainz
	// и ручными правками. Запрос стоит тут уже сейчас, чтобы потом
	// не искать, куда его вставить.
	err = tx.QueryRow(ctx,
		`SELECT artist_id FROM artist_aliases WHERE alias_key = $1`, nameKey).Scan(&id)
	if err == nil {
		cache[nameKey] = id
		return id, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("поиск псевдонима %q: %w", nameKey, err)
	}

	// Не нашли — заводим.
	//
	// ON CONFLICT здесь не для красоты: два трека одного исполнителя
	// подряд, и второй попытается вставить его снова. Кэш выше это
	// закрывает, но полагаться на кэш в вопросе целостности данных
	// неправильно — пусть база сама разрулит.
	err = tx.QueryRow(ctx, `
		INSERT INTO artists (name_raw, name_key)
		VALUES ($1, $2)
		ON CONFLICT (name_key) DO UPDATE SET updated_at = now()
		RETURNING id
	`, normalize.PrimaryArtist(rawName), nameKey).Scan(&id)
	if err != nil {
		return 0, false, fmt.Errorf("создание исполнителя %q: %w", nameKey, err)
	}

	cache[nameKey] = id
	return id, true, nil
}

// upsertTrack вставляет трек или обновляет уже существующий.
//
// Возвращает id трека и признак того, что трек новый.
func upsertTrack(
	ctx context.Context, tx pgx.Tx, libraryID, artistID int64, t dedupedTrack,
) (int64, bool, error) {

	// Длительность ноль означает "источник не сказал". В базе для этого
	// есть NULL, и это не то же самое, что ноль секунд. Поэтому
	// подставляем указатель: nil превратится в NULL.
	var duration *int
	if t.DurationSec > 0 {
		d := t.DurationSec
		duration = &d
	}

	var (
		id       int64
		inserted bool
	)

	// Разбор запроса ниже по частям.
	//
	// ON CONFLICT (library_id, artist_id, title_key) DO UPDATE — если трек
	// с таким естественным ключом уже есть, вместо ошибки обновляем его.
	//
	// ARRAY(SELECT DISTINCT unnest(...)) — объединение двух массивов
	// идентификаторов без повторов. unnest разворачивает массив в строки,
	// DISTINCT убирает дубли, ARRAY собирает обратно.
	//
	// (xmax = 0) AS inserted — приём, специфичный для PostgreSQL.
	// xmax — системная колонка: у только что вставленной строки она равна
	// нулю, у обновлённой — нет. Другого способа отличить вставку от
	// обновления в одном запросе PostgreSQL не даёт.
	err := tx.QueryRow(ctx, `
		INSERT INTO tracks (
			library_id, artist_id, title_raw, title_key,
			duration_sec, source_ids, is_present
		)
		VALUES ($1, $2, $3, $4, $5, $6::text[], TRUE)
		ON CONFLICT (library_id, artist_id, title_key) DO UPDATE
		   SET last_seen_at = now(),
		       is_present   = TRUE,
		       title_raw    = EXCLUDED.title_raw,
		       duration_sec = COALESCE(tracks.duration_sec, EXCLUDED.duration_sec),
		       source_ids   = ARRAY(
		           SELECT DISTINCT unnest(tracks.source_ids || EXCLUDED.source_ids)
		       )
		RETURNING id, (xmax = 0) AS inserted
	`,
		libraryID, artistID, t.Title, t.TitleKey,
		duration, t.SourceIDs,
	).Scan(&id, &inserted)
	if err != nil {
		return 0, false, fmt.Errorf("запись трека %q: %w", t.Title, err)
	}

	return id, inserted, nil
}

// ---------------------------------------------------------------------------
// Отчёты
// ---------------------------------------------------------------------------

// ImportRun — строка журнала импортов.
type ImportRun struct {
	ID         int64
	Library    string
	SourceName string
	SourceRef  string
	Status     string
	TracksSeen int
	TracksNew  int
	TracksGone int
	StartedAt  time.Time
	FinishedAt *time.Time
	ErrorText  string
}

// ImportRuns возвращает последние импорты, самые свежие первыми.
func (db *DB) ImportRuns(ctx context.Context, limit int) ([]ImportRun, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT r.id,
		       COALESCE(l.title, '(удалена)'),
		       r.source_name, r.source_ref, r.status,
		       r.tracks_seen, r.tracks_new, r.tracks_gone,
		       r.started_at, r.finished_at, r.error
		  FROM import_runs r
		  LEFT JOIN libraries l ON l.id = r.library_id
		 ORDER BY r.started_at DESC
		 LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("запрос журнала импортов: %w", err)
	}
	defer rows.Close()

	var out []ImportRun
	for rows.Next() {
		var r ImportRun
		if err := rows.Scan(
			&r.ID, &r.Library, &r.SourceName, &r.SourceRef, &r.Status,
			&r.TracksSeen, &r.TracksNew, &r.TracksGone,
			&r.StartedAt, &r.FinishedAt, &r.ErrorText,
		); err != nil {
			return nil, fmt.Errorf("чтение строки журнала: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LibraryStats — краткая сводка по библиотеке.
type LibraryStats struct {
	Title     string
	Tracks    int
	Artists   int
	Gone      int
	WithGenre int
}

// Stats собирает сводку по всем библиотекам.
func (db *DB) Stats(ctx context.Context) ([]LibraryStats, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT l.title,
		       count(*) FILTER (WHERE t.is_present)                    AS tracks,
		       count(DISTINCT t.artist_id) FILTER (WHERE t.is_present) AS artists,
		       count(*) FILTER (WHERE NOT t.is_present)                AS gone,
		       count(*) FILTER (
		           WHERE t.is_present AND EXISTS (
		               SELECT 1 FROM artist_genres ag WHERE ag.artist_id = t.artist_id
		           )
		       ) AS with_genre
		  FROM libraries l
		  LEFT JOIN tracks t ON t.library_id = l.id
		 GROUP BY l.id, l.title
		 ORDER BY l.title
	`)
	if err != nil {
		return nil, fmt.Errorf("запрос сводки: %w", err)
	}
	defer rows.Close()

	var out []LibraryStats
	for rows.Next() {
		var s LibraryStats
		if err := rows.Scan(&s.Title, &s.Tracks, &s.Artists, &s.Gone, &s.WithGenre); err != nil {
			return nil, fmt.Errorf("чтение строки сводки: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
