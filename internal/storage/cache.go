package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Здесь всё про provider_cache — реализация интерфейса
// enrich.CacheStore (internal/enrich/pipeline.go), только на SQL.
// storage о пакете enrich ничего не знает и не импортирует его — это
// pipeline.go описывает, что ему нужно, а *DB просто случайно этому
// соответствует (структурная типизация Go, как и everywhere в проекте).

// GetCache читает сохранённый ответ провайдера и решает, свежий ли он.
//
// ttl передаётся вызывающим кодом (enrich.CacheTTL), а не хранится здесь
// константой — *DB ничего не должен знать про то, что "30 дней" это именно
// срок жизни кэша Last.fm; для него это просто параметр запроса.
//
// Статус "error" НИКОГДА не считается свежим, даже моложе ttl. Подробное
// обоснование — в комментарии у CacheStore.GetCache в pipeline.go: ошибка
// доступа (например, геоблокировка без VPN, Р-017 в docs/DECISIONS.md) —
// это состояние окружения, а не устойчивый факт про исполнителя.
func (db *DB) GetCache(ctx context.Context, provider, requestKey string, ttl time.Duration) (raw []byte, status string, fresh bool, err error) {
	var fetchedAt time.Time
	err = db.Pool.QueryRow(ctx, `
		SELECT response, status, fetched_at
		  FROM provider_cache
		 WHERE provider = $1 AND request_key = $2
	`, provider, requestKey).Scan(&raw, &status, &fetchedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", false, nil
		}
		return nil, "", false, fmt.Errorf("чтение кэша (provider=%s, key=%s): %w", provider, requestKey, err)
	}

	fresh = status != "error" && time.Since(fetchedAt) < ttl
	return raw, status, fresh, nil
}

// PutCache сохраняет (или обновляет) ответ провайдера в кэше.
//
// response — колонка JSONB, она не примет произвольные байты, если это не
// валидный JSON. Обычно так и есть (Last.fm сам отвечает JSON-ом), но на
// случай пустого тела или неожиданного не-JSON ответа (например, HTML
// страницы ошибки 502 вместо тела API) — подстраховка ниже, чтобы запись
// в кэш не падала из-за формата данных, которые мы и так уже не смогли
// нормально разобрать.
func (db *DB) PutCache(ctx context.Context, provider, requestKey string, raw []byte, status string) error {
	body := raw
	switch {
	case len(body) == 0:
		body = []byte("null")
	case !json.Valid(body):
		wrapped, marshalErr := json.Marshal(string(body))
		if marshalErr != nil {
			return fmt.Errorf("подготовка не-JSON тела для кэша (provider=%s, key=%s): %w", provider, requestKey, marshalErr)
		}
		body = wrapped
	}

	_, err := db.Pool.Exec(ctx, `
		INSERT INTO provider_cache (provider, request_key, response, status, fetched_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (provider, request_key) DO UPDATE
		  SET response   = EXCLUDED.response,
		      status     = EXCLUDED.status,
		      fetched_at = now()
	`, provider, requestKey, body, status)
	if err != nil {
		return fmt.Errorf("запись кэша (provider=%s, key=%s): %w", provider, requestKey, err)
	}
	return nil
}
