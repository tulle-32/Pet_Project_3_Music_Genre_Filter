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
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/joho/godotenv"

	"github.com/tulle-32/music-genre-filter/internal/config"
	"github.com/tulle-32/music-genre-filter/internal/storage"
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

  genres tree [код]           показать дерево жанров
                              (без кода — всё дерево целиком)

Примеры:
  music db up
  music seed genres
  music genres tree rock
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
