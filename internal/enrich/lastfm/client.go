// Пакет lastfm — реализация enrich.GenreProvider поверх API Last.fm
// (метод artist.gettoptags).
//
// Пакет умеет ровно одну вещь: по имени исполнителя спросить у Last.fm
// его теги и вернуть их в виде []enrich.Tag. Ни кэширования (это
// provider_cache, отдельный слой — Р-005 в docs/DECISIONS.md), ни пауз
// между запросами (это забота вызывающего кода, который знает, сколько
// всего исполнителей нужно обогатить) здесь нет — каждая из этих вещей
// про СЕРИЮ запросов, а клиент знает только про ОДИН.
package lastfm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tulle-32/Pet_Project_3_Music_Genre_Filter/internal/enrich"
)

// defaultBaseURL — адрес API. Вынесен в константу, а не зашит в код запроса,
// чтобы тесты могли подменить его на адрес фальшивого сервера (httptest) —
// см. client_test.go. Обычным вызывающим кодом никогда не трогается.
const defaultBaseURL = "https://ws.audioscrobbler.com/2.0/"

// ErrAccessDenied возвращается, когда Last.fm отвечает кодом ошибки 11.
//
// Строго говоря, официально код 11 значит "Service Offline" — временная
// техническая неполадка. Но по факту (Р-017 в docs/DECISIONS.md,
// проверено вручную curl-ом) с текстом "Access Denied - You cannot access
// this service" этот же код приходит при географической блокировке API
// для российских адресов. Отличить одно от другого по одному лишь коду
// нельзя — оба раза в ответе будет 11. Поэтому ошибка называется по
// СИМПТОМУ ("доступ запрещён"), а не по официальному названию кода, и
// текст ошибки сразу подсказывает вероятную причину и решение (VPN),
// а не заставляет гадать заново то, что уже один раз выяснили руками.
var ErrAccessDenied = errors.New(
	"lastfm: доступ к API запрещён (error 11) — если ты в России, " +
		"проверь, включён ли VPN; подробности в docs/DECISIONS.md, Р-017")

// Client — клиент Last.fm API.
type Client struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
}

// New создаёт клиента с настоящим адресом API и разумным таймаутом.
//
// Таймаут 10 секунд, а не "без ограничений" (значение по умолчанию у
// http.Client) — намеренно. Без таймаута зависший запрос к чужому серверу
// подвесит и всю программу вместе с ним; лучше явная ошибка через 10
// секунд, чем бесконечное ожидание неизвестно чего.
func New(apiKey string) *Client {
	return &Client{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    defaultBaseURL,
	}
}

// Name возвращает машинное имя провайдера — попадает в колонку provider
// таблиц artist_genres, track_genres и provider_cache.
func (c *Client) Name() string {
	return "lastfm"
}

// apiResponse — то, что отдаёт artist.gettoptags, и в успешном, и
// в неуспешном случае сразу. Last.fm не использует разные структуры
// ответа или HTTP-коды для ошибок — почти всегда 200 OK, а разбираться,
// удался запрос или нет, нужно по полю Error внутри тела ответа.
type apiResponse struct {
	// Error и Message заполнены только при ошибке. Ноль в Error — успех.
	Error   int    `json:"error"`
	Message string `json:"message"`

	TopTags *struct {
		// Tag — самое неприятное место во всём разборе. Last.fm не
		// оборачивает поле в массив, если тег у исполнителя всего один:
		// вместо [{"name":"rock"}] придёт просто {"name":"rock"}, без
		// квадратных скобок. Обычный []tagDTO у encoding/json на такое
		// упадёт с ошибкой "cannot unmarshal object into Go value of
		// type []lastfm.tagDTO". Поэтому здесь json.RawMessage — "сырые
		// байты, разберём сами" — и разбор с проверкой обоих вариантов
		// вынесен в unmarshalTags ниже.
		Tag json.RawMessage `json:"tag"`
	} `json:"toptags"`
}

// tagDTO — один тег в том виде, в котором его прислал Last.fm.
// DTO = Data Transfer Object: структура ровно под форму чужого JSON,
// без единого поля сверху. Превращение в enrich.Tag — отдельный шаг.
type tagDTO struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// unmarshalTags разбирает поле "tag" ответа, зная, что оно может быть
// и массивом, и одиночным объектом (см. комментарий у apiResponse.TopTags).
func unmarshalTags(raw json.RawMessage) ([]tagDTO, error) {
	if len(raw) == 0 {
		// Тегов вообще нет — Last.fm иногда не присылает поле "tag"
		// совсем, если у исполнителя нет ни одного тега. Это не ошибка.
		return nil, nil
	}

	// Пробуем как массив — самый частый случай (два и больше тегов).
	var asSlice []tagDTO
	if err := json.Unmarshal(raw, &asSlice); err == nil {
		return asSlice, nil
	}

	// Не вышло — пробуем как одиночный объект (ровно один тег).
	var asSingle tagDTO
	if err := json.Unmarshal(raw, &asSingle); err == nil {
		return []tagDTO{asSingle}, nil
	}

	return nil, fmt.Errorf("lastfm: не удалось разобрать поле tag ни как список, ни как объект")
}

// TopTags реализует enrich.GenreProvider.
//
// Возвращает и разобранные теги, и сырые байты ответа — байты идут в кэш
// (provider_cache) на стороне пайплайна, см. подробное обоснование в
// комментарии у enrich.GenreProvider.TopTags. Само тело метода нарочно
// тонкое: сходить в сеть, прочитать байты, отдать их же на разбор
// ParseTopTags — ТОЙ ЖЕ функции, что потом разбирает эти байты и из кэша.
func (c *Client) TopTags(ctx context.Context, artistName string) (tags []enrich.Tag, raw []byte, err error) {
	reqURL := c.baseURL + "?" + url.Values{
		"method":  {"artist.gettoptags"},
		"artist":  {artistName},
		"api_key": {c.apiKey},
		"format":  {"json"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("lastfm: не удалось собрать запрос: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("lastfm: запрос не выполнен: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("lastfm: не удалось прочитать ответ: %w", err)
	}

	tags, err = c.ParseTopTags(body)
	if err != nil {
		// Байты отдаём наружу даже при ошибке разбора/доступа — пайплайн
		// сам решает, кэшировать ли их (например, ErrAccessDenied он
		// сознательно кэширует статусом "error", не "fresh" — Р-017).
		return nil, body, err
	}
	return tags, body, nil
}

// ParseTopTags разбирает СЫРОЕ тело ответа artist.gettoptags в []enrich.Tag.
//
// Метод (не отдельная функция пакета), чтобы *Client реализовывал её как
// часть enrich.GenreProvider — вызывается и изнутри TopTags на свежем
// ответе, и из пайплайна (internal/enrich/pipeline.go) на байтах, поднятых
// из provider_cache, БЕЗ единого различия в поведении между этими двумя
// путями: один и тот же код, один и тот же результат на одинаковых байтах.
func (c *Client) ParseTopTags(body []byte) ([]enrich.Tag, error) {
	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("lastfm: ответ не похож на JSON, который мы ждали: %w", err)
	}

	if parsed.Error != 0 {
		switch parsed.Error {
		case 6:
			// "The artist you supplied could not be found" — это не сбой,
			// а законный ответ "мы не знаем такого исполнителя". Пустой
			// срез без ошибки — именно то, что обещает enrich.GenreProvider.
			return nil, nil
		case 11:
			return nil, ErrAccessDenied
		default:
			return nil, fmt.Errorf("lastfm: ошибка API %d: %s", parsed.Error, parsed.Message)
		}
	}

	if parsed.TopTags == nil {
		return nil, nil
	}

	dtos, err := unmarshalTags(parsed.TopTags.Tag)
	if err != nil {
		return nil, err
	}

	tags := make([]enrich.Tag, 0, len(dtos))
	for _, dto := range dtos {
		tags = append(tags, enrich.Tag{Name: dto.Name, Weight: dto.Count})
	}
	return tags, nil
}
