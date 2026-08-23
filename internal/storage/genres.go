package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Здесь всё про справочник жанров: заливка из файла и чтение дерева.

// ---------------------------------------------------------------------------
// Разбор файла справочника
// ---------------------------------------------------------------------------

// genreCatalog — форма всего файла data/genre_map.json.
//
// Теги `json:"..."` говорят разборщику, как поле называется в файле.
// Поле Note нужно только чтобы разбор не спотыкался о пояснительный
// блок в начале файла; в коде оно нигде не используется.
type genreCatalog struct {
	Note   []string    `json:"_note"`
	Genres []genreNode `json:"genres"`
}

// genreNode — один жанр в файле.
//
// Обрати внимание: тип ссылается сам на себя через поле Children.
// Так описывается дерево произвольной глубины. Сейчас в файле два уровня,
// но формат готов к трём и больше, менять код для этого не придётся.
type genreNode struct {
	Code     string      `json:"code"`
	TitleRu  string      `json:"title_ru"`
	Aliases  []string    `json:"aliases"`
	Children []genreNode `json:"children"`
}

// SeedGenres читает файл справочника и заливает его в базу.
//
// Операция идемпотентная — это важное слово. Означает: сколько раз ни
// выполни, результат один и тот же. Второй запуск не наплодит дублей
// и не сломает уже проставленные жанры. Достигается за счёт
// ON CONFLICT DO UPDATE в запросах ниже.
//
// Возвращает количество залитых жанров и псевдонимов.
func (db *DB) SeedGenres(ctx context.Context, path string) (genres int, aliases int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("чтение файла справочника %s: %w", path, err)
	}

	var catalog genreCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return 0, 0, fmt.Errorf("разбор файла справочника %s: %w", path, err)
	}
	if len(catalog.Genres) == 0 {
		return 0, 0, fmt.Errorf("в файле %s нет ни одного жанра", path)
	}

	// Транзакция. Всё, что внутри, применяется целиком или не применяется
	// вовсе. Если на середине справочника случится ошибка, база останется
	// в том же виде, что была до вызова, — не будет состояния «половина
	// жанров залилась, половина нет».
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("начало транзакции: %w", err)
	}
	// Rollback после успешного Commit — не ошибка, а безвредная операция.
	// Поэтому его можно смело откладывать через defer: при удачном исходе
	// он ничего не сделает, при неудачном — откатит всё сделанное.
	defer func() { _ = tx.Rollback(ctx) }()

	// Рекурсивный обход дерева. Функция объявлена переменной, потому что
	// обычная вложенная функция в Go не может вызвать сама себя: на момент
	// объявления её имени ещё не существует. Приём стандартный: сначала
	// объявляем переменную нужного типа, потом присваиваем ей тело.
	var walk func(nodes []genreNode, parentID *int32) error
	walk = func(nodes []genreNode, parentID *int32) error {
		for _, node := range nodes {
			if node.Code == "" || node.TitleRu == "" {
				return fmt.Errorf("у жанра пустой code или title_ru: %+v", node)
			}

			var id int32
			// ON CONFLICT (code) DO UPDATE — «вставь, а если такой code уже
			// есть, то обнови». В SQL это называется upsert. Без него
			// пришлось бы сначала делать SELECT, потом решать, INSERT это
			// или UPDATE, и ловить состояние гонки между этими двумя шагами.
			//
			// EXCLUDED — служебное имя строки, которую мы пытались вставить.
			// То есть «возьми новое значение вместо старого».
			err := tx.QueryRow(ctx, `
				INSERT INTO genres (code, title_ru, parent_id)
				VALUES ($1, $2, $3)
				ON CONFLICT (code) DO UPDATE
				  SET title_ru  = EXCLUDED.title_ru,
				      parent_id = EXCLUDED.parent_id
				RETURNING id
			`, node.Code, node.TitleRu, parentID).Scan(&id)
			if err != nil {
				return fmt.Errorf("запись жанра %s: %w", node.Code, err)
			}
			genres++

			// Псевдонимы. Ключ приводим к нижнему регистру и обрезаем
			// пробелы — ровно так же, как потом будем нормализовывать
			// теги, приходящие от провайдеров. Если эти две нормализации
			// разойдутся, справочник перестанет находиться.
			for _, alias := range node.Aliases {
				key := strings.ToLower(strings.TrimSpace(alias))
				if key == "" {
					continue
				}
				_, err := tx.Exec(ctx, `
					INSERT INTO genre_aliases (alias_key, genre_id, source)
					VALUES ($1, $2, 'seed')
					ON CONFLICT (alias_key) DO UPDATE
					  SET genre_id = EXCLUDED.genre_id
				`, key, id)
				if err != nil {
					return fmt.Errorf("запись псевдонима %q: %w", key, err)
				}
				aliases++
			}

			// Сам код жанра тоже делаем псевдонимом самого себя.
			// Тогда тег "rock", пришедший от провайдера, найдётся, даже
			// если его забыли перечислить в aliases.
			selfKey := strings.ToLower(node.Code)
			_, err = tx.Exec(ctx, `
				INSERT INTO genre_aliases (alias_key, genre_id, source)
				VALUES ($1, $2, 'seed')
				ON CONFLICT (alias_key) DO NOTHING
			`, selfKey, id)
			if err != nil {
				return fmt.Errorf("запись собственного псевдонима %q: %w", selfKey, err)
			}

			// Спускаемся к детям, передавая им свой id как родительский.
			if len(node.Children) > 0 {
				if err := walk(node.Children, &id); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// Верхний уровень дерева: родителя нет, поэтому nil.
	if err := walk(catalog.Genres, nil); err != nil {
		return 0, 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("фиксация транзакции: %w", err)
	}
	return genres, aliases, nil
}

// ---------------------------------------------------------------------------
// Чтение дерева
// ---------------------------------------------------------------------------

// GenreTreeRow — одна строка результата обхода дерева.
type GenreTreeRow struct {
	ID      int32
	Code    string
	TitleRu string
	Depth   int32  // 0 у корня, 1 у его детей и так далее
	Path    string // путь от корня: "rock > punk_rock"
	Tracks  int64  // сколько треков сейчас относится к этому жанру
	Aliases int64  // сколько псевдонимов заведено
}

// GenreTree возвращает жанр и всё его поддерево.
//
// Если rootCode пустой — возвращает всё дерево целиком.
//
// Это тот самый рекурсивный запрос, ради которого мы ушли с SQLite.
// Разберём, как он работает, потому что конструкция непривычная.
//
//	WITH RECURSIVE tree AS (
//	    <якорь>          -- строки, с которых начинаем
//	  UNION ALL
//	    <шаг>            -- как из уже найденных строк получить следующие
//	)
//
// База сначала выполняет якорь и складывает результат в tree. Потом
// выполняет шаг, подставляя туда то, что нашла на предыдущем круге.
// И повторяет, пока очередной круг не вернёт ноль новых строк.
//
// То есть «начни с рока, найди его детей, потом детей этих детей,
// и так пока не кончится» — одним запросом, без цикла в приложении.
func (db *DB) GenreTree(ctx context.Context, rootCode string) ([]GenreTreeRow, error) {
	const query = `
WITH RECURSIVE tree AS (
    -- ЯКОРЬ: с чего начинаем.
    -- Либо конкретный жанр по коду, либо все корневые (у кого нет родителя).
    SELECT g.id,
           g.code,
           g.title_ru,
           0::int             AS depth,
           g.code::text       AS path
    FROM genres g
    WHERE ($1 = '' AND g.parent_id IS NULL)
       OR ($1 <> '' AND g.code = $1)

    UNION ALL

    -- ШАГ: дети тех, кого уже нашли.
    -- Соединяем таблицу genres с промежуточным результатом tree.
    SELECT child.id,
           child.code,
           child.title_ru,
           parent.depth + 1,
           parent.path || ' > ' || child.code
    FROM genres child
    JOIN tree parent ON child.parent_id = parent.id
)
SELECT t.id,
       t.code,
       t.title_ru,
       t.depth,
       t.path,
       -- Подзапросы-счётчики. Для справочника из полусотни строк это
       -- совершенно нормально; на больших объёмах их заменяют
       -- соединением с группировкой.
       (SELECT count(*) FROM artist_genres ag
          JOIN tracks tr ON tr.artist_id = ag.artist_id
         WHERE ag.genre_id = t.id AND tr.is_present)          AS tracks,
       (SELECT count(*) FROM genre_aliases ga
         WHERE ga.genre_id = t.id)                            AS aliases
FROM tree t
ORDER BY t.path;
`

	rows, err := db.Pool.Query(ctx, query, rootCode)
	if err != nil {
		return nil, fmt.Errorf("запрос дерева жанров: %w", err)
	}
	// rows держит открытым соединение из пула. Забыть его закрыть —
	// значит рано или поздно исчерпать пул и подвесить приложение.
	// defer прямо здесь избавляет от необходимости об этом помнить.
	defer rows.Close()

	var result []GenreTreeRow
	for rows.Next() {
		var r GenreTreeRow
		// Scan раскладывает колонки строки по переменным. Порядок
		// аргументов должен совпадать с порядком колонок в SELECT.
		// Знак & передаёт адрес переменной, чтобы Scan мог в неё записать.
		if err := rows.Scan(&r.ID, &r.Code, &r.TitleRu, &r.Depth, &r.Path, &r.Tracks, &r.Aliases); err != nil {
			return nil, fmt.Errorf("чтение строки дерева: %w", err)
		}
		result = append(result, r)
	}
	// Ошибка могла случиться и в процессе перебора, а не только при Scan.
	// rows.Err() — единственный способ о ней узнать; пропустить эту проверку
	// значит однажды молча получить неполный список.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("перебор строк дерева: %w", err)
	}

	if len(result) == 0 && rootCode != "" {
		return nil, fmt.Errorf("жанр с кодом %q не найден", rootCode)
	}
	return result, nil
}

// CountGenres возвращает, сколько жанров и псевдонимов сейчас в базе.
// Нужна, чтобы после заливки было чем проверить результат.
func (db *DB) CountGenres(ctx context.Context) (genres int64, aliases int64, err error) {
	err = db.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM genres),
		       (SELECT count(*) FROM genre_aliases)
	`).Scan(&genres, &aliases)
	if err != nil {
		return 0, 0, fmt.Errorf("подсчёт жанров: %w", err)
	}
	return genres, aliases, nil
}
