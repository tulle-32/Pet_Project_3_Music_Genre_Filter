package storage

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Этот файл — про управление уже существующими библиотеками: посмотреть,
// что есть, и поправить, если ошиблись при вводе.
//
// Появился он из конкретного случая: человек набирал название библиотеки
// руками в командной строке и не заметил, что часть букв не напечаталась
// ("ус-ан" вместо "Рус-Лан"). Импорт в findOrCreateLibrary (import.go) не
// умеет спрашивать "ты уверен?" — он честно создаёт то, что ему передали.
// Значит, нужен отдельный, простой способ это исправить постфактум.
//
// Сознательно НЕ делаем здесь: слияние двух библиотек в одну (если
// опечатка привела к тому, что часть треков легла не туда, треки внутри
// не переезжают), нечёткий поиск похожих названий, отмену переименования.
// Всё это откладываем до v1.1.0, где будет отдельная задача на "ручные
// правки" — сейчас нужно решить только исходную проблему: дать точному,
// осознанно введённому названию замениться на другое точное название.

// LibraryInfo — одна строка из таблицы libraries, для показа человеку.
type LibraryInfo struct {
	ID         int64
	Title      string
	SourceName string
	SourceRef  string
	CreatedAt  time.Time
}

// Libraries возвращает все библиотеки с их точными названиями.
//
// Существует прежде всего как ответ на вопрос "а как у меня записано
// на самом деле?" — на глаз в терминале "Рус-Лан" и "Рус-лан" неотличимы,
// а для базы это два разных названия.
func (db *DB) Libraries(ctx context.Context) ([]LibraryInfo, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, title, source_name, source_ref, created_at
		  FROM libraries
		 ORDER BY title
	`)
	if err != nil {
		return nil, fmt.Errorf("запрос списка библиотек: %w", err)
	}
	defer rows.Close()

	var out []LibraryInfo
	for rows.Next() {
		var l LibraryInfo
		if err := rows.Scan(&l.ID, &l.Title, &l.SourceName, &l.SourceRef, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("чтение строки библиотеки: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// RenameLibrary меняет название библиотеки со старого на новое.
//
// Совпадение старого названия ищется ТОЧНОЕ, посимвольное — умышленно.
// Нечёткий поиск ("может, ты имел в виду...") звучит удобно, но именно
// он и создал бы риск переименовать не ту библиотеку молча. Если название
// не найдено, ошибка прямо предлагает посмотреть точный список.
func (db *DB) RenameLibrary(ctx context.Context, oldTitle, newTitle string) error {
	oldTitle = strings.TrimSpace(oldTitle)
	newTitle = strings.TrimSpace(newTitle)

	if oldTitle == "" || newTitle == "" {
		return fmt.Errorf("название библиотеки не может быть пустым")
	}
	if oldTitle == newTitle {
		return fmt.Errorf("новое название совпадает со старым, менять нечего")
	}

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начало транзакции: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Проверяем заранее, что новое название свободно. Без этой проверки
	// UPDATE ниже просто переименовал бы в дубликат — libraries.title
	// не имеет уникального ограничения (см. комментарий в findOrCreateLibrary,
	// import.go), поэтому база сама такую ошибку не покажет, а два разных
	// исполнителя внутри одной "библиотеки" с двумя строками — это как раз
	// та путаница, слияние из которой мы сознательно не делаем сейчас.
	var exists bool
	err = tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM libraries WHERE title = $1)`, newTitle,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("проверка нового названия: %w", err)
	}
	if exists {
		return fmt.Errorf(
			"библиотека с названием %q уже существует; "+
				"объединение библиотек пока не сделано (запланировано на v1.1.0), "+
				"выбери другое название", newTitle)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE libraries SET title = $2, updated_at = now() WHERE title = $1`,
		oldTitle, newTitle)
	if err != nil {
		return fmt.Errorf("переименование библиотеки: %w", err)
	}

	// RowsAffected() == 0 означает "WHERE ничего не нашёл", а не ошибку
	// самого запроса — pgx в этом случае err не вернёт. Проверяем отдельно,
	// иначе человек решит, что переименование прошло, хотя оно тихо
	// ничего не сделало.
	if tag.RowsAffected() == 0 {
		return fmt.Errorf(
			"библиотека с названием %q не найдена; "+
				"посмотри точные названия: music library list", oldTitle)
	}

	return tx.Commit(ctx)
}
