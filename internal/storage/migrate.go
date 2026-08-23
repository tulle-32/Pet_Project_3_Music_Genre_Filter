package storage

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // регистрирует драйвер "pgx" в database/sql
	"github.com/pressly/goose/v3"

	"github.com/tulle-32/music-genre-filter/migrations"
)

// Здесь живёт всё, что связано с миграциями схемы.
//
// Что такое миграция и зачем она. Во втором проекте ты менял схему SQLite
// руками через ALTER TABLE. Это работает ровно до момента, когда баз
// становится две: у тебя на компьютере и на сервере. Тогда надо помнить,
// какие изменения куда уже применены, и порядок, в котором их применяли.
//
// Миграция — это способ хранить изменения схемы в файлах и в git, рядом
// с кодом, который на эту схему рассчитан. Инструмент ведёт в самой базе
// служебную таблицу goose_db_version: какие файлы уже накатаны. Дальше он
// просто применяет недостающие по порядку. Схема перестаёт быть чем-то,
// что живёт в голове, и становится частью репозитория.

// MigrateUp применяет все ещё не применённые миграции.
func MigrateUp(ctx context.Context, dsn string) error {
	db, err := openStdlib(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("накат миграций: %w", err)
	}
	return nil
}

// MigrateDown откатывает ОДНУ последнюю миграцию.
//
// Именно одну, а не все: массовый откат слишком легко запустить случайно,
// а он удаляет таблицы вместе с данными. Хочешь откатить три — вызови три раза
// и каждый раз подумай.
func MigrateDown(ctx context.Context, dsn string) error {
	db, err := openStdlib(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := goose.DownContext(ctx, db, "."); err != nil {
		return fmt.Errorf("откат миграции: %w", err)
	}
	return nil
}

// MigrateStatus печатает, какие миграции применены, а какие ещё нет.
func MigrateStatus(ctx context.Context, dsn string) error {
	db, err := openStdlib(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := goose.StatusContext(ctx, db, "."); err != nil {
		return fmt.Errorf("статус миграций: %w", err)
	}
	return nil
}

// openStdlib открывает соединение с базой в виде *sql.DB и настраивает goose.
//
// Тут есть тонкость, ради которой стоит остановиться. В приложении мы
// работаем через pgxpool — родной интерфейс pgx, быстрый и удобный.
// Но goose, как и большинство библиотек в экосистеме, написан под
// стандартный интерфейс database/sql из стандартной библиотеки Go.
//
// Пакет pgx/stdlib — это переходник: он оборачивает pgx так, что снаружи
// тот выглядит обычным драйвером database/sql. Тот же приём, что и во всей
// экосистеме Go: договорились об интерфейсе, а кто за ним стоит — дело
// десятое.
//
// Соединение здесь отдельное и живёт только на время миграции. Смешивать
// его с рабочим пулом приложения незачем: миграции запускаются редко
// и отдельной командой.
func openStdlib(dsn string) (*sql.DB, error) {
	// "pgx" — имя драйвера, под которым pgx/stdlib регистрируется
	// в database/sql. Регистрация происходит сама, в момент импорта
	// пакета stdlib выше: у него есть функция init(), которая выполняется
	// при загрузке программы. Поэтому импорт помечен подчёркиванием —
	// сам пакет мы по имени не используем, нам нужен только его побочный
	// эффект. В Go это единственный законный случай "импорта ради эффекта".
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("открытие соединения для миграций: %w", err)
	}

	// Говорим goose, что база — PostgreSQL. У разных баз разный синтаксис
	// служебной таблицы версий, поэтому диалект указывать обязательно.
	if err := goose.SetDialect("postgres"); err != nil {
		db.Close()
		return nil, fmt.Errorf("выбор диалекта: %w", err)
	}

	// Отдаём goose встроенные в бинарник файлы миграций вместо папки на диске.
	goose.SetBaseFS(migrations.FS)

	return db, nil
}
