// Пакет config отвечает ровно за одну вещь: собрать все настройки приложения
// из переменных окружения в одну структуру и проверить, что они осмысленные.
//
// Зачем отдельный пакет под такую мелочь. Настройки нужны почти везде: адрес
// базы — слою хранения, адрес порта — веб-серверу, ключи API — обогащению.
// Если каждый из них будет сам дёргать os.Getenv, то:
//   - нигде не будет единого места, где видно ВСЕ настройки проекта;
//   - опечатку в имени переменной ты найдёшь только в момент падения;
//   - в тестах невозможно подсунуть другие значения, не трогая окружение.
//
// Поэтому правило: os.Getenv вызывается ТОЛЬКО здесь. Все остальные пакеты
// получают готовую структуру Config и не знают, откуда взялись значения.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — все настройки приложения в одном месте.
//
// В Go структура (struct) — это просто набор именованных полей, как запись
// в таблице. Заглавная буква в имени поля (HTTPAddr, а не httpAddr) означает
// "экспортируемое": видно из других пакетов. Со строчной буквы — приватное,
// видно только внутри этого пакета. Это вся система доступа в Go, никаких
// public/private/protected.
type Config struct {
	// HTTPAddr — на каком адресе слушает веб-сервер.
	// Формат "host:port". Пустой host, как в ":8080", означает
	// "слушать на всех сетевых интерфейсах" — именно это нужно в контейнере,
	// иначе снаружи до сервиса не достучаться.
	HTTPAddr string

	// LogLevel — с какого уровня писать сообщения в журнал.
	// В разработке удобно debug, в бою обычно info.
	LogLevel slog.Level

	// ShutdownTimeout — сколько ждать завершения текущих запросов,
	// когда пришёл сигнал остановиться. Подробности — в cmd/server/main.go.
	ShutdownTimeout time.Duration

	// Database — настройки подключения к PostgreSQL. Вложенная структура,
	// чтобы не сваливать десяток полей в одну кучу.
	Database DatabaseConfig
}

// DatabaseConfig — всё, что нужно, чтобы подключиться к базе.
type DatabaseConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string

	// MaxConns — сколько одновременных подключений к базе держим в пуле.
	// Пул — это заранее открытые соединения, которые переиспользуются.
	// Открывать новое соединение на каждый запрос дорого: это сетевое
	// рукопожатие плюс аутентификация, десятки миллисекунд на пустом месте.
	MaxConns int32
}

// DSN собирает строку подключения в формате, который понимает драйвер pgx.
//
// DSN расшифровывается как Data Source Name — исторически сложившееся
// название строки "где база и как в неё войти".
//
// Синтаксис "func (c DatabaseConfig) DSN() string" читается так: это функция
// DSN, привязанная к типу DatabaseConfig. В других языках это назвали бы
// методом класса. Кусок "(c DatabaseConfig)" называется получателем
// (receiver) — внутри функции переменная c указывает на конкретный экземпляр.
func (c DatabaseConfig) DSN() string {
	// sslmode=disable — потому что база живёт в контейнере на том же
	// компьютере, шифровать трафик самому себе смысла нет. Когда база
	// переедет на отдельный сервер, это нужно будет поменять на require.
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=disable",
		c.User, c.Password, c.Host, c.Port, c.Name,
	)
}

// SafeDSN — то же самое, но с паролем, заменённым на звёздочки.
// Нужна для журналов: строку подключения полезно видеть при отладке,
// но пароль в журнале — это тот самый секрет, который потом всплывает
// в чужих руках. Правило простое: в журнал попадает только SafeDSN.
func (c DatabaseConfig) SafeDSN() string {
	return fmt.Sprintf(
		"postgres://%s:****@%s:%d/%s?sslmode=disable",
		c.User, c.Host, c.Port, c.Name,
	)
}

// Load читает переменные окружения и собирает из них Config.
//
// Возвращает два значения: саму конфигурацию и ошибку. Это основной способ
// сообщать об ошибках в Go — не исключения, а второе возвращаемое значение.
// Звёздочка в "*Config" означает указатель: возвращаем не копию структуры,
// а адрес, где она лежит. Для больших структур так дешевле.
func Load() (*Config, error) {
	// mustGet* функции ниже складывают свои претензии сюда, чтобы показать
	// пользователю ВСЕ проблемы разом, а не по одной за запуск.
	var problems []string

	cfg := &Config{
		HTTPAddr:        getString("HTTP_ADDR", ":8080"),
		ShutdownTimeout: getDuration("SHUTDOWN_TIMEOUT", 10*time.Second, &problems),
		LogLevel:        getLogLevel("LOG_LEVEL", slog.LevelInfo, &problems),

		Database: DatabaseConfig{
			Host:     getString("POSTGRES_HOST", "localhost"),
			Port:     getInt("POSTGRES_PORT", 5432, &problems),
			User:     getRequired("POSTGRES_USER", &problems),
			Password: getRequired("POSTGRES_PASSWORD", &problems),
			Name:     getRequired("POSTGRES_DB", &problems),
			MaxConns: int32(getInt("POSTGRES_MAX_CONNS", 10, &problems)),
		},
	}

	// len(problems) — длина среза. Срез (slice) в Go это список переменной
	// длины, примерно как массив в других языках.
	if len(problems) > 0 {
		// strings.Join склеивает список строк через разделитель.
		return nil, fmt.Errorf("настройки заполнены неверно:\n  - %s",
			strings.Join(problems, "\n  - "))
	}

	return cfg, nil
}

// ---------------------------------------------------------------------------
// Ниже — маленькие вспомогательные функции. Все со строчной буквы, то есть
// приватные: снаружи пакета их не видно и не нужно.
// ---------------------------------------------------------------------------

// getString читает переменную окружения. Если она пустая или не задана —
// возвращает значение по умолчанию.
func getString(key, fallback string) string {
	// os.LookupEnv возвращает два значения: само значение и признак того,
	// была ли переменная вообще задана. Это важное различие: пустая строка
	// и отсутствующая переменная — не одно и то же.
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

// getRequired читает обязательную переменную. Если её нет — записывает
// претензию в список проблем и возвращает пустую строку.
//
// Аргумент "problems *[]string" — указатель на срез. Указатель нужен,
// чтобы функция могла ДОПОЛНИТЬ список вызывающей стороны. Если передать
// срез без указателя, функция получит копию и добавление потеряется.
func getRequired(key string, problems *[]string) string {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		*problems = append(*problems, fmt.Sprintf(
			"переменная %s не задана (посмотри .env.example)", key))
		return ""
	}
	return value
}

// getInt читает число. Если текст не превращается в число — это ошибка
// настройки, а не повод молча подставить значение по умолчанию: пусть
// человек увидит, что написал ерунду.
func getInt(key string, fallback int, problems *[]string) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw) // Atoi = ASCII to integer
	if err != nil {
		*problems = append(*problems, fmt.Sprintf(
			"переменная %s должна быть числом, а там %q", key, raw))
		return fallback
	}
	return value
}

// getDuration читает промежуток времени в человекочитаемом виде:
// "10s", "1m30s", "500ms". Такой формат понимает time.ParseDuration.
func getDuration(key string, fallback time.Duration, problems *[]string) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}

	value, err := time.ParseDuration(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf(
			"переменная %s должна быть промежутком времени вида 10s или 1m, а там %q", key, raw))
		return fallback
	}
	return value
}

// getLogLevel превращает слово в уровень журналирования.
func getLogLevel(key string, fallback slog.Level, problems *[]string) slog.Level {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}

	// switch в Go не требует break в конце каждой ветки — он не проваливается
	// в следующую, в отличие от C и Java.
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		*problems = append(*problems, fmt.Sprintf(
			"переменная %s должна быть одной из debug/info/warn/error, а там %q", key, raw))
		return fallback
	}
}
