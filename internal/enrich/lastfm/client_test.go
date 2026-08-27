package lastfm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient создаёт клиента, который стучится не в настоящий Last.fm,
// а в переданный тестовый сервер. baseURL — поле неэкспортируемое, но тест
// лежит в том же пакете (lastfm, не lastfm_test), поэтому видит его напрямую.
// Благодаря этому тесты не ходят в сеть вообще — они пройдут одинаково
// что дома, что в GitHub Actions, что за VPN, что без него (Р-017).
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	c := New("test-api-key")
	c.baseURL = server.URL
	return c
}

func TestTopTags_Success(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"toptags": {
				"tag": [
					{"name": "rock", "count": 100},
					{"name": "grunge", "count": 80}
				]
			}
		}`))
	})

	tags, _, err := c.TopTags(context.Background(), "Nirvana")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("ожидали 2 тега, получили %d: %+v", len(tags), tags)
	}
	if tags[0].Name != "rock" || tags[0].Weight != 100 {
		t.Errorf("первый тег разобран неверно: %+v", tags[0])
	}
	if tags[1].Name != "grunge" || tags[1].Weight != 80 {
		t.Errorf("второй тег разобран неверно: %+v", tags[1])
	}
}

// TestTopTags_SingleTagAsObject проверяет самую неприятную особенность
// формата Last.fm: когда тег ровно один, поле "tag" — не массив из одного
// элемента, а голый объект. См. комментарий у apiResponse.TopTags в client.go.
func TestTopTags_SingleTagAsObject(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"toptags": {
				"tag": {"name": "soundtrack", "count": 42}
			}
		}`))
	})

	tags, _, err := c.TopTags(context.Background(), "Some One-Tag Artist")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(tags) != 1 {
		t.Fatalf("ожидали 1 тег, получили %d: %+v", len(tags), tags)
	}
	if tags[0].Name != "soundtrack" || tags[0].Weight != 42 {
		t.Errorf("тег разобран неверно: %+v", tags[0])
	}
}

func TestTopTags_NoTags(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"toptags": {}}`))
	})

	tags, _, err := c.TopTags(context.Background(), "Artist Without Tags")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("ожидали пустой список, получили: %+v", tags)
	}
}

// TestTopTags_ArtistNotFound: код ошибки 6 — законный ответ "не знаем
// такого исполнителя", а не сбой. Контракт enrich.GenreProvider обещает
// в этом случае пустой список без ошибки.
func TestTopTags_ArtistNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error": 6, "message": "The artist you supplied could not be found"}`))
	})

	tags, _, err := c.TopTags(context.Background(), "Совсем Неизвестный Артист")
	if err != nil {
		t.Fatalf("код 6 не должен превращаться в ошибку, получили: %v", err)
	}
	if tags != nil {
		t.Errorf("ожидали nil, получили: %+v", tags)
	}
}

// TestTopTags_AccessDenied — ровно то, что мы поймали руками в Р-017:
// код 11 с текстом про Access Denied, а не про временную неполадку.
func TestTopTags_AccessDenied(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error": 11, "message": "Access Denied - You cannot access this service"}`))
	})

	_, _, err := c.TopTags(context.Background(), "Nirvana")
	if err != ErrAccessDenied {
		t.Fatalf("ожидали ErrAccessDenied, получили: %v", err)
	}
}

func TestTopTags_OtherAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error": 29, "message": "You have exceeded your rate limit"}`))
	})

	_, _, err := c.TopTags(context.Background(), "Nirvana")
	if err == nil {
		t.Fatal("ожидали ошибку, получили nil")
	}
}
