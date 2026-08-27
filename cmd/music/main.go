// Команда music — утилита командной строки для административных операций.
//
// Что здесь и зачем. Сервис из cmd/server отвечает на HTTP-запросы и живёт
// постоянно. А есть операции, которые запускают руками или по расписанию:
// накатить миграции, залить справочник, посмотреть, что в базе. Их место
// в отдельной программе — не потому что «так принято», а по практической
// причине: сервису не нужны эти команды, а команде не нужен веб-сервер.
//
// Обе программы пользуются одним и тем же кодом из internal/. Разница
// только в том, как этот код запускается.
//
// Разбор аргументов сделан вручную, обычным switch, без библиотеки вроде
// cobra. Команд пока пять, и весь разбор укладывается в полсотни строк,
// которые видно целиком. Когда команд станет два десятка и появятся флаги
// у каждой — тогда и подключим cobra, это её честная область применения.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/joho/godotenv"

	"github.com/tulle-32/Pet_Project_3_Music_Genre_Filter/internal/config"
	"github.com/tulle-32/Pet_Project_3_Music_Genre_Filter/internal/enrich"
	"github.com/tulle-32/Pet_Project_3_Music_Genre_Filter/internal/enrich/lastfm"
	"github.com/tulle-32/Pet_Project_3_Music_Genre_Filter/internal/normalize"
	"github.com/tulle-32/Pet_Project_3_Music_Genre_Filter/internal/sources/file"
	"github.com/tulle-32/Pet_Project_3_Music_Genre_Filter/internal/storage"
)

// usage — текст справки. Печатается без аргументов и при ошибке.
const usage = `music — утилита управления Music Genre Filter

Использование:
  music <команда> [аргументы]

Команды:
  db up                       накатить миграции схемы
  db down                     откатить ОДНУ последнюю миграцию
  db status                   показать, какие миграции применены

  seed genres [файл]          залить справочник жанров
                              (по умолчанию data/genre_map.json)

  import file <путь>          загрузить треки из файла .json или .csv
       [--library "Имя"]      в какую библиотеку класть
                              (по умолчанию "Моя музыка")
                              треки из ВК тоже приходят сюда — файлом,
                              который выгружает расширение браузера
                              extension/ (Р-016, docs/VK_ACCESS.md)
  import list                 история импортов

  library list                показать точные названия библиотек
  library rename <старое> <новое>
                              переименовать библиотеку (например,
                              если опечатался при импорте)

  genres tree [код]           показать дерево жанров
                              (без кода — всё дерево целиком)

  enrich run                  определить жанры исполнителей через Last.fm
       [--limit N]            обработать не больше N исполнителей за прогон
                              (по умолчанию — все, у кого есть треки)
                              требует LASTFM_API_KEY в .env; если ты в
                              России — включи VPN перед запуском (Р-017,
                              docs/DECISIONS.md)
  enrich list                 история прогонов обогащения

  filter <код-жанра>          показать треки библиотеки по жанру
       --library "Имя"        (обязательно — какую библиотеку смотрим)
       [--min 0.15]           минимальная уверенность, 0..1
                              (по умолчанию 0.15 — с одним провайдером
                              Last.fm 0.50 из docs/ARCHITECTURE.md
                              недостижимо, см. Р-018/Р-019 в DECISIONS.md)
                              поджанры попадают в выдачу автоматически:
                              "rock" заодно приносит punk_rock, hard_rock и т.д.

  stats                       сводка по библиотекам

Примеры:
  music db up
  music seed genres
  music import file data/samples/sample_tracks.json --library "Рус-Лан"
  music import file tracks.json --library "Рус-Лан"
  music library list
  music library rename "ус-ан" "Рус-Лан"
  music genres tree rock
  music enrich run --limit 50
  music filter rock --library "Рус-Лан"
  music stats
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nОшибка: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// os.Args[0] — путь к самой программе, поэтому аргументы начинаются
	// с первого элемента, а не с нулевого.
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Тот же перехват сигналов, что и в сервере: если команда долгая,
	// Ctrl+C должен её аккуратно прервать, а не убить посреди транзакции.
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := cfg.Database.DSN()

	// Разбор команды. Первый аргумент — группа, второй — действие внутри неё.
	// Такая двухуровневая схема (как у docker или kubectl) читается лучше,
	// чем два десятка команд в плоском списке.
	switch args[0] {

	case "db":
		if len(args) < 2 {
			return fmt.Errorf("после db нужно указать up, down или status")
		}
		switch args[1] {
		case "up":
			fmt.Println("Накатываю миграции...")
			if err := storage.MigrateUp(ctx, dsn); err != nil {
				return err
			}
			fmt.Println("Готово.")
			return nil

		case "down":
			fmt.Println("Откатываю последнюю миграцию...")
			if err := storage.MigrateDown(ctx, dsn); err != nil {
				return err
			}
			fmt.Println("Готово.")
			return nil

		case "status":
			return storage.MigrateStatus(ctx, dsn)

		default:
			return fmt.Errorf("неизвестная команда db %q", args[1])
		}

	case "seed":
		if len(args) < 2 || args[1] != "genres" {
			return fmt.Errorf("пока умею только: seed genres")
		}
		path := "data/genre_map.json"
		if len(args) >= 3 {
			path = args[2]
		}
		return seedGenres(ctx, cfg, path)

	case "import":
		if len(args) < 2 {
			return fmt.Errorf("после import нужно указать file или list")
		}
		switch args[1] {
		case "file":
			if len(args) < 3 {
				return fmt.Errorf("укажи путь к файлу: music import file data/samples/sample_tracks.json")
			}
			path := args[2]
			library := flagValue(args[3:], "--library", "Моя музыка")
			return importFile(ctx, cfg, path, library)

		case "list":
			return importList(ctx, cfg)

		default:
			return fmt.Errorf("неизвестная команда import %q", args[1])
		}

	case "library":
		if len(args) < 2 {
			return fmt.Errorf("после library нужно указать list или rename")
		}
		switch args[1] {
		case "list":
			return libraryList(ctx, cfg)

		case "rename":
			if len(args) < 4 {
				return fmt.Errorf(
					`нужно два названия: music library rename "старое" "новое"`)
			}
			return libraryRename(ctx, cfg, args[2], args[3])

		default:
			return fmt.Errorf("неизвестная команда library %q", args[1])
		}

	case "enrich":
		if len(args) < 2 {
			return fmt.Errorf("после enrich нужно указать run или list")
		}
		switch args[1] {
		case "run":
			limit := 0 // 0 = без ограничения
			if v := flagValue(args[2:], "--limit", ""); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("--limit должен быть числом, получили %q", v)
				}
				limit = n
			}
			return enrichRun(ctx, cfg, limit)

		case "list":
			return enrichList(ctx, cfg)

		default:
			return fmt.Errorf("неизвестная команда enrich %q", args[1])
		}

	case "filter":
		if len(args) < 2 {
			return fmt.Errorf("укажи код жанра: music filter rock --library \"Имя\"")
		}
		genreCode := args[1]
		library := flagValue(args[2:], "--library", "")
		if library == "" {
			return fmt.Errorf(`нужна библиотека: music filter %s --library "Имя" (точные названия: music library list)`, genreCode)
		}
		minConfidence := defaultMinConfidence
		if v := flagValue(args[2:], "--min", ""); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("--min должен быть числом от 0 до 1, получили %q", v)
			}
			minConfidence = f
		}
		return filterTracks(ctx, cfg, genreCode, library, minConfidence)

	case "stats":
		return showStats(ctx, cfg)

	case "genres":
		if len(args) < 2 || args[1] != "tree" {
			return fmt.Errorf("пока умею только: genres tree [код]")
		}
		root := ""
		if len(args) >= 3 {
			root = args[2]
		}
		return genresTree(ctx, cfg, root)

	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil

	default:
		fmt.Print(usage)
		return fmt.Errorf("неизвестная команда %q", args[0])
	}
}

// ---------------------------------------------------------------------------
// Реализация команд
// ---------------------------------------------------------------------------

func seedGenres(ctx context.Context, cfg *config.Config, path string) error {
	db, err := storage.New(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	fmt.Printf("Заливаю справочник из %s...\n", path)

	genres, aliases, err := db.SeedGenres(ctx, path)
	if err != nil {
		return err
	}

	total, totalAliases, err := db.CountGenres(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("Обработано: %d жанров, %d псевдонимов.\n", genres, aliases)
	fmt.Printf("Сейчас в базе: %d жанров, %d псевдонимов.\n", total, totalAliases)
	fmt.Println("\nПовторный запуск ничего не сломает: записи обновляются, а не дублируются.")
	return nil
}

func genresTree(ctx context.Context, cfg *config.Config, root string) error {
	db, err := storage.New(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.GenreTree(ctx, root)
	if err != nil {
		return err
	}

	// tabwriter выравнивает колонки по ширине самой длинной ячейки.
	// Работает просто: ты разделяешь колонки символом \t, а он потом
	// расставляет пробелы. Без него вывод с кириллицей разной длины
	// превращается в лесенку.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ЖАНР\tКОД\tТРЕКОВ\tПСЕВДОНИМОВ")

	for _, r := range rows {
		// Отступ по глубине — так дерево видно глазами.
		indent := strings.Repeat("  ", int(r.Depth))
		fmt.Fprintf(w, "%s%s\t%s\t%d\t%d\n",
			indent, r.TitleRu, r.Code, r.Tracks, r.Aliases)
	}

	// Flush обязателен: до него tabwriter копит строки у себя и ничего
	// не печатает — ему надо увидеть все строки, чтобы понять ширину колонок.
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\nВсего строк: %d\n", len(rows))
	if root == "" {
		fmt.Println("Показано всё дерево. Для одной ветки: music genres tree rock")
	}
	return nil
}

// flagValue достаёт значение флага из оставшихся аргументов.
//
// Понимает обе привычные записи: "--library Имя" и "--library=Имя".
// Своя реализация вместо пакета flag нужна потому, что стандартный flag
// требует, чтобы флаги шли ДО позиционных аргументов, а нам удобнее
// писать путь к файлу первым: "import file dump.json --library X".
func flagValue(args []string, name, fallback string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"=")
		}
	}
	return fallback
}

// importFile загружает треки из файла в библиотеку.
func importFile(ctx context.Context, cfg *config.Config, path, library string) error {
	db, err := storage.New(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	// Вот она, польза от интерфейса. Здесь создаётся файловый источник,
	// но переменная src имеет тип TrackSource. Когда в v0.6.0 появится
	// источник ВК, изменится ровно одна строка — та, что ниже. Всё
	// остальное в этой функции и во всём импорте останется как есть.
	src := file.New(path)

	fmt.Printf("Читаю %s...\n", src.Ref())
	raw, err := src.Fetch(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Прочитано строк: %d\n\n", len(raw))

	fmt.Printf("Заливаю в библиотеку %q...\n", library)
	res, err := db.ImportTracks(ctx, library, src, raw)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "\nСтрок в источнике:\t%d\n", res.FromSource)
	fmt.Fprintf(w, "Повторов внутри файла:\t%d\n", res.Duplicates)
	fmt.Fprintf(w, "Уникальных треков:\t%d\n", res.Unique)
	fmt.Fprintf(w, "Из них новых в базе:\t%d\n", res.NewTracks)
	fmt.Fprintf(w, "Новых исполнителей:\t%d\n", res.NewArtists)
	fmt.Fprintf(w, "Пропали с прошлого раза:\t%d\n", res.Gone)
	fmt.Fprintf(w, "Заняло:\t%s\n", res.Took.Round(time.Millisecond))
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Println("\nПовторный импорт того же файла ничего не сломает:")
	fmt.Println("треки найдутся по ключу и просто обновят дату последней встречи.")
	return nil
}

// importList печатает историю импортов.
func importList(ctx context.Context, cfg *config.Config) error {
	db, err := storage.New(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	runs, err := db.ImportRuns(ctx, 20)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("Импортов ещё не было.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "КОГДА\tБИБЛИОТЕКА\tИСТОЧНИК\tСТАТУС\tТРЕКОВ\tНОВЫХ\tПРОПАЛО")
	for _, r := range runs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\n",
			r.StartedAt.Local().Format("02.01 15:04"),
			r.Library, r.SourceName, r.Status,
			r.TracksSeen, r.TracksNew, r.TracksGone)
	}
	return w.Flush()
}

// libraryList печатает точные названия всех библиотек.
//
// Появилась именно из-за опечаток: в терминале "Рус-Лан" и "ус-ан" видно
// сразу, а вот "Рус-Лан" и "Рус-лан" (строчная "л") на глаз почти не
// различить. Здесь названия просто печатаются как есть, без выравнивания
// "на глаз" — если нужно увидеть невидимые пробелы, название можно
// обернуть в кавычки в самом терминале.
func libraryList(ctx context.Context, cfg *config.Config) error {
	db, err := storage.New(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	libs, err := db.Libraries(ctx)
	if err != nil {
		return err
	}
	if len(libs) == 0 {
		fmt.Println("Библиотек ещё нет. Загрузи треки: music import file <путь>")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tНАЗВАНИЕ\tИСТОЧНИК\tСОЗДАНА")
	for _, l := range libs {
		fmt.Fprintf(w, "%d\t%q\t%s\t%s\n",
			l.ID, l.Title, l.SourceName, l.CreatedAt.Local().Format("02.01.2006 15:04"))
	}
	return w.Flush()
}

// libraryRename переименовывает библиотеку.
//
// Название нужно указывать ТОЧНО так, как оно лежит в базе — команда
// сознательно не пытается угадывать и не ищет похожие варианты (почему —
// см. комментарий у storage.RenameLibrary). Если не уверен, как оно
// записано на самом деле: music library list.
func libraryRename(ctx context.Context, cfg *config.Config, oldTitle, newTitle string) error {
	db, err := storage.New(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.RenameLibrary(ctx, oldTitle, newTitle); err != nil {
		return err
	}

	fmt.Printf("Готово: %q → %q.\n", oldTitle, newTitle)
	fmt.Println("Треки и жанры внутри библиотеки не тронуты — изменилось только название.")
	return nil
}

// enrichCourtesyDelay — пауза между сетевыми запросами к Last.fm.
//
// Last.fm нигде не публикует числовой лимит запросов в секунду — в
// документации так и написано: ограничения применяются "на усмотрение"
// сервиса. Явного числа нет, поэтому взята разумная осторожная пауза,
// а не "сколько выдержит сеть": так прогон на сотни исполнителей не
// выглядит для чужого сервера атакой и не рискует словить бан по IP
// (что было бы куда хуже одной лишней ошибки ErrAccessDenied).
//
// Пауза выдерживается только после НАСТОЯЩЕГО сетевого запроса — если
// ответ пришёл из кэша (res.FromCache), ждать незачем, сеть и так не
// использовалась.
const enrichCourtesyDelay = 250 * time.Millisecond

// enrichRun обходит исполнителей библиотек и определяет им жанры через
// Last.fm: кэш → провайдер → теги → уверенность (internal/enrich).
//
// limit == 0 значит "без ограничения — обработать всех". Ограничение
// удобно для первого пробного прогона на маленьком куске, прежде чем
// запускать на всей библиотеке.
func enrichRun(ctx context.Context, cfg *config.Config, limit int) error {
	if cfg.LastFM.APIKey == "" {
		return fmt.Errorf(
			"не задан LASTFM_API_KEY — добавь ключ в .env " +
				"(как получить ключ: docs/DECISIONS.md, раздел про Last.fm)")
	}

	db, err := storage.New(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	artists, err := db.ListArtists(ctx)
	if err != nil {
		return err
	}
	if limit > 0 && limit < len(artists) {
		artists = artists[:limit]
	}
	if len(artists) == 0 {
		fmt.Println("Обогащать некого: нет исполнителей с треками в библиотеках.")
		return nil
	}

	fmt.Printf("Исполнителей к обработке: %d\n", len(artists))
	fmt.Println("Если ты в России — проверь, включён ли VPN (Р-017, docs/DECISIONS.md).")
	fmt.Println()

	runID, err := db.StartEnrichRun(ctx)
	if err != nil {
		return err
	}

	provider := lastfm.New(cfg.LastFM.APIKey)
	stats := storage.EnrichRunStats{ArtistsTotal: len(artists)}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ИСПОЛНИТЕЛЬ\tЖАНРОВ\tИСТОЧНИК")

	var abortErr error
	for i, a := range artists {
		if err := ctx.Err(); err != nil {
			abortErr = err
			break
		}

		artistName := normalize.PrimaryArtist(a.NameRaw)
		res, err := enrich.EnrichArtist(ctx, provider, db, db, db, a.ID, artistName, a.NameKey)
		if err != nil {
			// ErrAccessDenied (Р-017) — не проблема КОНКРЕТНОГО исполнителя,
			// а состояние всей сети целиком (пропал VPN и т.п.). Продолжать
			// перебирать остальных исполнителей в этом случае бессмысленно:
			// каждый следующий вызов провалится точно так же, а квоту
			// запросов и время мы потратим впустую. Поэтому прогон
			// прерывается сразу, а не тонет в сотне одинаковых ошибок.
			if errors.Is(err, lastfm.ErrAccessDenied) {
				abortErr = err
				break
			}

			stats.ArtistsFailed++
			fmt.Fprintf(w, "%s\tошибка\t%v\n", a.NameRaw, err)
			continue
		}

		stats.ArtistsEnriched++
		if res.FromCache {
			stats.CacheHits++
		} else {
			stats.APICalls++
		}

		source := "сеть"
		if res.FromCache {
			source = "кэш"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\n", a.NameRaw, res.MatchedGenres, source)

		// Пауза только после настоящего сетевого запроса и только если
		// это не последний исполнитель — незачем ждать после самого
		// последнего вызова, дальше всё равно делать нечего.
		if !res.FromCache && i < len(artists)-1 {
			time.Sleep(enrichCourtesyDelay)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	status := "ok"
	errText := ""
	if abortErr != nil {
		status = "failed"
		errText = abortErr.Error()
	}
	if err := db.FinishEnrichRun(ctx, runID, status, stats, errText); err != nil {
		return err
	}

	fmt.Printf("\nОбработано: %d, обогащено: %d, ошибок: %d.\n",
		stats.ArtistsTotal, stats.ArtistsEnriched, stats.ArtistsFailed)
	fmt.Printf("Запросов в сеть: %d, из кэша: %d.\n", stats.APICalls, stats.CacheHits)

	if abortErr != nil {
		return fmt.Errorf("прогон прерван: %w", abortErr)
	}
	return nil
}

// enrichList печатает историю прогонов обогащения.
func enrichList(ctx context.Context, cfg *config.Config) error {
	db, err := storage.New(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	runs, err := db.EnrichRuns(ctx, 20)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("Обогащений ещё не было. Запусти: music enrich run")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "КОГДА\tСТАТУС\tВСЕГО\tОБОГАЩЕНО\tОШИБОК\tСЕТЬ\tКЭШ")
	for _, r := range runs {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\n",
			r.StartedAt.Local().Format("02.01 15:04"), r.Status,
			r.ArtistsTotal, r.ArtistsEnriched, r.ArtistsFailed,
			r.APICalls, r.CacheHits)
	}
	return w.Flush()
}

// showStats печатает сводку по библиотекам.
func showStats(ctx context.Context, cfg *config.Config) error {
	db, err := storage.New(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	stats, err := db.Stats(ctx)
	if err != nil {
		return err
	}
	if len(stats) == 0 {
		fmt.Println("Библиотек ещё нет. Загрузи треки: music import file <путь>")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "БИБЛИОТЕКА\tТРЕКОВ\tИСПОЛНИТЕЛЕЙ\tС ЖАНРОМ\tПРОПАЛО")
	for _, s := range stats {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\n",
			s.Title, s.Tracks, s.Artists, s.WithGenre, s.Gone)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Println("\nЕсли «с жанром» меньше «исполнителей» — запусти: music enrich run")
	return nil
}

// filterTracks печатает треки библиотеки, относящиеся к жанру genreCode
// (вместе со всеми его поджанрами) с уверенностью не ниже minConfidence.
//
// Это и есть исходная задача всего проекта: вставил жанр → получил список.
// Пока без экспорта в файл или плейлист — только терминал; печатать в файл
// можно и обычным перенаправлением шелла (`> список.txt`), отдельная логика
// экспорта для этого не нужна.
// defaultMinConfidence — порог уверенности по умолчанию для `music filter`.
//
// В docs/ARCHITECTURE.md записан порог 0.50 — но он рассчитан на формулу
// с ТРЕМЯ провайдерами сразу (musicbrainz 0.50 + lastfm 0.35 + deezer 0.15,
// сумма). После Р-018 (docs/DECISIONS.md) реализован только Last.fm с весом
// 0.35 — то есть 0.50 физически недостижимо: ни один тег никогда не наберёт
// такую уверенность, и `filter` с порогом по умолчанию находил бы ноль
// треков всегда, при любых данных. Это не тонкая настройка, а прямая
// поломка команды.
//
// 0.15 — компромисс для одного провайдера: у Last.fm вес тега (count)
// нормирован так, что топовый тег исполнителя получает 100, а более редкие
// тегом — меньше. Порог 0.15 соответствует тегу с весом ~43 из 100 и выше —
// то есть заметному, а не случайному тегу, но не обязательно САМОМУ
// популярному. Число подобрано на глаз, а не выведено из данных: если на
// практике фильтр будет находить слишком много (или слишком мало) —
// поправить можно на месте флагом --min, без пересборки.
const defaultMinConfidence = 0.15

func filterTracks(ctx context.Context, cfg *config.Config, genreCode, library string, minConfidence float64) error {
	db, err := storage.New(ctx, cfg.Database.DSN(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	tracks, err := db.FilterTracks(ctx, library, genreCode, minConfidence)
	if err != nil {
		return err
	}
	if len(tracks) == 0 {
		fmt.Printf("Ничего не нашлось: жанр %q, библиотека %q, уверенность ≥ %.2f.\n",
			genreCode, library, minConfidence)
		fmt.Println("Возможные причины: обогащение ещё не запускалось (music enrich run)," +
			" порог слишком высокий (--min пониже), или в библиотеке правда нет такого жанра.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ИСПОЛНИТЕЛЬ\tТРЕК\tДЛИТЕЛЬНОСТЬ\tУВЕРЕННОСТЬ")
	for _, t := range tracks {
		duration := "—"
		if t.DurationSec > 0 {
			duration = fmt.Sprintf("%d:%02d", t.DurationSec/60, t.DurationSec%60)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%.2f\n", t.Artist, t.Title, duration, t.Confidence)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\nНайдено треков: %d.\n", len(tracks))
	return nil
}
