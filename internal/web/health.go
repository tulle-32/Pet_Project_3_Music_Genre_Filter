package web

import (
	"encoding/json"
	"net/http"
	"time"
)

// healthResponse — форма ответа эндпоинта /health.
//
// Строки в обратных кавычках после типа поля называются тегами. Пакет
// encoding/json читает их, чтобы понять, как назвать поле в JSON.
// Без тега поле Status превратилось бы в "Status" с заглавной буквы,
// а в JSON принято писать со строчной. omitempty означает "не выводить
// поле вовсе, если оно пустое".
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Uptime  string `json:"uptime"`
}

// readyResponse — форма ответа эндпоинта /ready.
type readyResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
	Error    string `json:"error,omitempty"`
}

// handleHealth отвечает на вопрос "процесс жив?".
//
// Никуда не ходит и ничего не проверяет — сознательно. Если этот эндпоинт
// начнёт зависеть от базы, то при недоступной базе оркестратор решит, что
// приложение мертво, и убьёт его. А оно живо, просто ждёт базу. Убивать
// его в этот момент — ровно то, чего делать не надо.
//
// Сигнатура func(w http.ResponseWriter, r *http.Request) — стандартная
// для всех обработчиков в Go. w это то, куда пишем ответ, r — входящий запрос.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: s.version,
		// Round убирает наносекунды: "1m3s" читается лучше, чем "1m3.048291733s".
		Uptime: time.Since(s.started).Round(time.Second).String(),
	})
}

// handleReady отвечает на вопрос "приложение готово работать?".
//
// В отличие от /health, здесь мы реально дёргаем базу. Если она не отвечает,
// возвращаем 503 Service Unavailable — общепринятый код "я жив, но пока
// не могу обслуживать запросы".
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	// r.Context() — контекст запроса. Он отменяется автоматически, если
	// клиент оборвал соединение. Передавая его дальше в базу, мы получаем
	// приятное свойство: клиент отвалился — проверка базы тоже прекращается,
	// а не доводится до конца впустую.
	if err := s.db.Health(r.Context()); err != nil {
		s.log.Warn("проверка готовности не прошла", "ошибка", err)

		writeJSON(w, http.StatusServiceUnavailable, readyResponse{
			Status:   "not ready",
			Database: "недоступна",
			Error:    err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, readyResponse{
		Status:   "ready",
		Database: "доступна",
	})
}

// writeJSON — маленький помощник: превращает любое значение в JSON
// и отправляет с нужным кодом ответа.
//
// Тип "any" это псевдоним для interface{} — "значение любого типа".
// Появился в Go 1.18, чтобы не писать пустой интерфейс глазами.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	// Заголовок Content-Type ОБЯЗАН быть установлен до WriteHeader.
	// После того как код ответа ушёл клиенту, заголовки менять поздно —
	// они уже в сети. Это частая ошибка новичков в Go: заголовок ставят
	// после записи тела и потом долго ищут, почему браузер видит text/plain.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	// SetEscapeHTML(false) нужен, чтобы кириллица и символы вроде < >
	// не превращались в <. Читаемость ответа для человека здесь важнее
	// перестраховки от вставки JSON в HTML-страницу, которой у нас нет.
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	// Ошибку записи сознательно игнорируем: если запись в сетевое соединение
	// не удалась, значит клиент уже ушёл, и сообщить ему об этом всё равно
	// нечем. Подчёркивание — способ явно сказать "я знаю про это значение
	// и намеренно его выбрасываю". Просто проигнорировать Go не даст.
	_ = enc.Encode(payload)
}
