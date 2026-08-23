-- Первая миграция: вся схема базы целиком.
--
-- Как читать этот файл. Ниже встретятся две строки-разметки: они начинаются
-- как обычный комментарий, но содержат имя инструмента и слово Up или Down.
-- Это не комментарии, а команды для goose. Всё между ними выполняется при
-- накате версии, всё после второй — при откате.
--
-- Кстати, живой пример на ровном месте: разметку нельзя упоминать в тексте
-- дважды в одной строке — разборщик goose честно попытается её выполнить
-- и скажет "multiple annotations". Поэтому здесь она описана словами.
--
-- Правило, которое стоит запомнить: миграции НЕ редактируются после того,
-- как их применили. Если схему надо поменять — пишется следующая миграция.
-- Иначе у тебя на компьютере база одна, а на сервере другая, и никто уже
-- не помнит, почему.

-- +goose Up

-- ============================================================================
-- СЫРОЕ ИЗ ИСТОЧНИКА
-- ============================================================================

-- libraries — чья это коллекция треков.
--
-- Пока библиотека будет одна, и колонка owner_user_id всегда пустая.
-- Таблица заведена заранее сознательно: добавить её потом дёшево, а вот
-- добавить library_id в уже заполненную tracks — это миграция с переносом
-- данных и сменой уникального ключа. См. docs/DECISIONS.md, запись Р-006.
CREATE TABLE libraries (
    id            BIGSERIAL   PRIMARY KEY,
    title         TEXT        NOT NULL,

    -- Владелец. NULL до появления пользователей в v2.0.0.
    owner_user_id BIGINT,

    -- Откуда приехала: 'file', 'vk', 'archive'.
    source_name   TEXT        NOT NULL,
    -- Что именно: имя файла или ссылка на страницу.
    source_ref    TEXT        NOT NULL DEFAULT '',

    -- CHECK — ограничение уровня базы. Оно не даст записать в колонку
    -- что-то кроме перечисленного, даже если в коде будет ошибка.
    -- Проверять такие вещи в базе надёжнее, чем в приложении: приложений
    -- может быть несколько, а база одна.
    visibility    TEXT        NOT NULL DEFAULT 'private'
                              CHECK (visibility IN ('private', 'link', 'public')),

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE libraries IS 'Коллекция треков одного человека';

-- artists — исполнители. Общие на всё приложение, а не на библиотеку.
--
-- Это важное решение. Если у тебя и у друга есть Нирвана — это ОДНА запись
-- в artists. Значит и жанр для неё определяется один раз, на всех. Чем
-- больше библиотек залито, тем дешевле обходится каждая следующая.
CREATE TABLE artists (
    id         BIGSERIAL   PRIMARY KEY,

    -- Как было написано в источнике, во всей красе: "КиШ", "  Nirvana ".
    name_raw   TEXT        NOT NULL,

    -- Нормализованный ключ для сравнения: нижний регистр, без лишних
    -- пробелов и знаков. Именно по нему ищем совпадения.
    -- UNIQUE гарантирует, что один и тот же исполнитель не заведётся дважды.
    name_key   TEXT        NOT NULL UNIQUE,

    -- Идентификатор в MusicBrainz, если удалось сопоставить.
    -- Тип UUID, а не TEXT: база сама проверит формат и будет хранить
    -- компактнее (16 байт вместо 36 символов).
    mbid       UUID,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE artists IS 'Исполнители, общие для всех библиотек';

-- artist_aliases — разные написания одного исполнителя.
--
-- Нужна из-за кириллицы: "Король и Шут", "КиШ" и "Korol i Shut" — один
-- коллектив, но нормализация даст три разных ключа. Сюда складываем все
-- варианты, чтобы при следующем импорте они попали в того же исполнителя.
CREATE TABLE artist_aliases (
    alias_key  TEXT        PRIMARY KEY,
    artist_id  BIGINT      NOT NULL REFERENCES artists (id) ON DELETE CASCADE,
    source     TEXT        NOT NULL DEFAULT 'import'
                           CHECK (source IN ('import', 'manual', 'musicbrainz')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Индекс на внешний ключ. PostgreSQL НЕ создаёт их автоматически, в отличие
-- от некоторых других баз. Без индекса запрос "все псевдонимы исполнителя X"
-- будет перебирать таблицу целиком.
CREATE INDEX artist_aliases_artist_id_idx ON artist_aliases (artist_id);

-- tracks — собственно треки.
CREATE TABLE tracks (
    id            BIGSERIAL   PRIMARY KEY,

    -- ON DELETE CASCADE: удалили библиотеку — её треки уехали следом.
    library_id    BIGINT      NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,

    -- ON DELETE RESTRICT: а вот удалить исполнителя, у которого есть треки,
    -- база не даст. Это защита от случайной потери данных.
    artist_id     BIGINT      NOT NULL REFERENCES artists (id) ON DELETE RESTRICT,

    title_raw     TEXT        NOT NULL,
    title_key     TEXT        NOT NULL,

    duration_sec  INTEGER     CHECK (duration_sec IS NULL OR duration_sec > 0),

    -- Массив — тип, которого в SQLite нет вовсе.
    -- Зачем он здесь: в источнике один и тот же трек часто лежит несколько
    -- раз с разными идентификаторами. Мы схлопываем их в одну строку,
    -- а все исходные идентификаторы складываем сюда.
    source_ids    TEXT[]      NOT NULL DEFAULT '{}',

    -- Трек всё ещё в библиотеке. При повторной синхронизации пропавшие
    -- треки не удаляются, а помечаются FALSE — так видно, что человек убрал.
    is_present    BOOLEAN     NOT NULL DEFAULT TRUE,

    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Естественный ключ трека. Именно по нему при импорте решается,
    -- новый это трек или уже виденный.
    UNIQUE (library_id, artist_id, title_key)
);

CREATE INDEX tracks_artist_id_idx ON tracks (artist_id);

-- Частичный индекс — ещё одна вещь, которой в SQLite нет.
-- Условие WHERE в конце означает: индексировать только присутствующие треки.
-- Индекс получается меньше и быстрее, потому что удалённые треки в него
-- не попадают, а искать мы почти всегда будем среди присутствующих.
CREATE INDEX tracks_library_present_idx ON tracks (library_id) WHERE is_present;

-- ============================================================================
-- СПРАВОЧНИК ЖАНРОВ
-- ============================================================================

-- genres — канонические жанры с иерархией.
--
-- parent_id ссылается на эту же таблицу. Такая структура называется
-- самоссылающейся, и именно она позволяет запросу "покажи рок" автоматически
-- захватить панк-рок, хард-рок и гранж — через рекурсивный запрос.
CREATE TABLE genres (
    id        SERIAL  PRIMARY KEY,

    -- Машинное имя: латиницей, без пробелов. По нему обращаются запросы.
    code      TEXT    NOT NULL UNIQUE,

    -- Человеческое имя для показа.
    title_ru  TEXT    NOT NULL,

    parent_id INTEGER REFERENCES genres (id) ON DELETE RESTRICT,

    -- Простейшая защита от жанра, который родитель сам себе.
    -- От длинных циклов (A → B → A) это не спасёт, но такую петлю
    -- ещё надо суметь создать руками.
    CHECK (parent_id IS NULL OR parent_id <> id)
);

CREATE INDEX genres_parent_id_idx ON genres (parent_id);

COMMENT ON TABLE genres IS 'Дерево жанров: parent_id указывает на родителя';

-- genre_aliases — соответствие "тег провайдера → наш жанр".
--
-- Провайдеры отдают народную разметку: рядом с 'rock' будет 'хард-рок',
-- 'alt-rock', 'seen live' и 'favourite'. Эта таблица переводит первые три
-- в наши коды, а последние две просто не находит — и они уходят в отчёт
-- о нераспознанных тегах, из которого справочник растёт руками.
CREATE TABLE genre_aliases (
    alias_key TEXT    PRIMARY KEY,
    genre_id  INTEGER NOT NULL REFERENCES genres (id) ON DELETE CASCADE,
    source    TEXT    NOT NULL DEFAULT 'seed'
                      CHECK (source IN ('seed', 'manual'))
);

CREATE INDEX genre_aliases_genre_id_idx ON genre_aliases (genre_id);

-- ============================================================================
-- РЕЗУЛЬТАТ СОПОСТАВЛЕНИЯ
-- ============================================================================

-- artist_genres — какие жанры приписаны исполнителю и кем.
--
-- Обрати внимание на состав первичного ключа: в него входит provider.
-- Это значит, что мнение каждого источника хранится ОТДЕЛЬНО. MusicBrainz
-- сказал "рок" с весом 0.9, Last.fm сказал "рок" с весом 0.7 — это две
-- разные строки. Так всегда видно, откуда взялась итоговая цифра.
CREATE TABLE artist_genres (
    artist_id  BIGINT       NOT NULL REFERENCES artists (id) ON DELETE CASCADE,
    genre_id   INTEGER      NOT NULL REFERENCES genres (id) ON DELETE CASCADE,
    provider   TEXT         NOT NULL,

    -- NUMERIC(4,3) — число с фиксированной точностью: 4 знака всего,
    -- 3 после запятой. То есть от 0.000 до 9.999, а CHECK сужает до 0..1.
    -- Почему не FLOAT: у чисел с плавающей точкой 0.1 + 0.2 не равно 0.3,
    -- и сравнения вида "confidence >= 0.5" начинают вести себя странно.
    confidence NUMERIC(4,3) NOT NULL CHECK (confidence >= 0 AND confidence <= 1),

    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now(),

    PRIMARY KEY (artist_id, genre_id, provider)
);

-- Индекс для главного запроса проекта: "дай все треки жанра X".
-- Он идёт от жанра к исполнителям, то есть в обратную сторону от ключа.
CREATE INDEX artist_genres_genre_id_idx ON artist_genres (genre_id);

-- track_genres — то же самое, но для конкретного трека.
--
-- Нужна там, где жанра исполнителя недостаточно: акустический альбом
-- у рок-группы, фит рэпера с металлистами. Заполняется точечно.
CREATE TABLE track_genres (
    track_id   BIGINT       NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    genre_id   INTEGER      NOT NULL REFERENCES genres (id) ON DELETE CASCADE,
    provider   TEXT         NOT NULL,
    confidence NUMERIC(4,3) NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now(),

    PRIMARY KEY (track_id, genre_id, provider)
);

CREATE INDEX track_genres_genre_id_idx ON track_genres (genre_id);

-- overrides — ручные правки. Применяются последними и всегда побеждают.
--
-- Здесь сознательное отступление от правил: у target_id НЕТ внешнего ключа.
-- Причина в том, что он указывает то на artists, то на tracks — в зависимости
-- от scope. Такие ссылки на две таблицы сразу база проверить не умеет.
-- Плата за это: следить за целостностью придётся приложению. Взамен таблица
-- остаётся одна вместо двух почти одинаковых.
CREATE TABLE overrides (
    id         BIGSERIAL   PRIMARY KEY,

    scope      TEXT        NOT NULL CHECK (scope IN ('artist', 'track')),
    target_id  BIGINT      NOT NULL,

    genre_id   INTEGER     NOT NULL REFERENCES genres (id) ON DELETE CASCADE,

    -- 'add' — считать этот жанр своим, даже если алгоритм не согласен.
    -- 'remove' — не считать, даже если алгоритм уверен.
    action     TEXT        NOT NULL CHECK (action IN ('add', 'remove')),

    note       TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (scope, target_id, genre_id, action)
);

CREATE INDEX overrides_lookup_idx ON overrides (genre_id, action);

-- ============================================================================
-- ЖУРНАЛЫ И КЭШ
-- ============================================================================

-- provider_cache — сырые ответы внешних API как есть.
--
-- JSONB — двоичный JSON, ещё один тип, которого нет в SQLite. Хранится
-- в разобранном виде, поэтому по нему можно искать и строить индексы,
-- не разбирая текст каждый раз заново.
--
-- Зачем хранить сырой ответ, а не только результат разбора: когда мы
-- поменяем логику разбора тегов (а мы поменяем), можно будет пересчитать
-- жанры из кэша, вообще не обращаясь в сеть.
CREATE TABLE provider_cache (
    provider    TEXT        NOT NULL,

    -- Ключ запроса: 'lastfm:artist.getTopTags:nirvana'.
    request_key TEXT        NOT NULL,

    response    JSONB       NOT NULL,

    -- Отрицательный результат тоже кэшируем: если исполнителя нет
    -- в MusicBrainz, незачем спрашивать про него каждый раз.
    status      TEXT        NOT NULL CHECK (status IN ('ok', 'not_found', 'error')),

    fetched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (provider, request_key)
);

-- Для вычистки протухшего кэша по сроку.
CREATE INDEX provider_cache_fetched_at_idx ON provider_cache (fetched_at);

-- import_runs — история импортов.
CREATE TABLE import_runs (
    id          BIGSERIAL   PRIMARY KEY,

    -- ON DELETE SET NULL: библиотеку удалили, но запись в журнале
    -- остаётся — история не должна исчезать вместе с данными.
    library_id  BIGINT      REFERENCES libraries (id) ON DELETE SET NULL,

    source_name TEXT        NOT NULL,
    source_ref  TEXT        NOT NULL DEFAULT '',

    status      TEXT        NOT NULL DEFAULT 'running'
                            CHECK (status IN ('running', 'ok', 'failed')),

    tracks_seen INTEGER     NOT NULL DEFAULT 0,
    tracks_new  INTEGER     NOT NULL DEFAULT 0,
    tracks_gone INTEGER     NOT NULL DEFAULT 0,

    error       TEXT        NOT NULL DEFAULT '',
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX import_runs_started_at_idx ON import_runs (started_at DESC);

-- enrich_runs — история обогащения жанрами.
CREATE TABLE enrich_runs (
    id               BIGSERIAL   PRIMARY KEY,

    status           TEXT        NOT NULL DEFAULT 'running'
                                 CHECK (status IN ('running', 'ok', 'failed')),

    artists_total    INTEGER     NOT NULL DEFAULT 0,
    artists_enriched INTEGER     NOT NULL DEFAULT 0,
    artists_failed   INTEGER     NOT NULL DEFAULT 0,

    -- Эти два счётчика покажут, работает ли кэш. Если api_calls растёт
    -- при каждом прогоне, а cache_hits нет — что-то сломано.
    api_calls        INTEGER     NOT NULL DEFAULT 0,
    cache_hits       INTEGER     NOT NULL DEFAULT 0,

    error            TEXT        NOT NULL DEFAULT '',
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at      TIMESTAMPTZ
);

CREATE INDEX enrich_runs_started_at_idx ON enrich_runs (started_at DESC);

-- +goose Down

-- Откат. Порядок обратный созданию: сначала то, что ссылается на других,
-- потом то, на что ссылаются. Иначе база откажется удалять таблицу,
-- на которую ещё указывают внешние ключи.
DROP TABLE IF EXISTS enrich_runs;
DROP TABLE IF EXISTS import_runs;
DROP TABLE IF EXISTS provider_cache;
DROP TABLE IF EXISTS overrides;
DROP TABLE IF EXISTS track_genres;
DROP TABLE IF EXISTS artist_genres;
DROP TABLE IF EXISTS genre_aliases;
DROP TABLE IF EXISTS genres;
DROP TABLE IF EXISTS tracks;
DROP TABLE IF EXISTS artist_aliases;
DROP TABLE IF EXISTS artists;
DROP TABLE IF EXISTS libraries;
