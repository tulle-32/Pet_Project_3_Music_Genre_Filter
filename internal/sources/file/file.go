// Пакет file — источник треков из файла на диске.
//
// Поддерживает два формата: JSON и CSV. Формат определяется по расширению.
//
// Это первая реализация интерфейса TrackSource и одновременно рабочая
// лошадка проекта: на ней разрабатывается и тестируется вся обработка,
// не завися от живого ВКонтакте. Даже когда появится источник ВК, файловый
// никуда не денется — он останется запасным путём на случай, когда обходной
// путь к ВК перестанет работать.
package file

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tulle-32/music-genre-filter/internal/sources"
)

// Source — источник треков из файла.
//
// Поля со строчной буквы, то есть приватные: снаружи пакета их не видно.
// Создать Source можно только через New, и это правильно — так нельзя
// получить наполовину настроенный объект.
type Source struct {
	path string
}

// New создаёт файловый источник.
func New(path string) *Source {
	return &Source{path: path}
}

// Проверка на этапе компиляции, что Source действительно реализует
// интерфейс TrackSource.
//
// Читается так: "присвоить пустую структуру Source переменной типа
// TrackSource, а результат выбросить". Переменная с именем _ никуда
// не сохраняется, но само присваивание компилятор проверяет. Если завтра
// кто-то поменяет сигнатуру метода в интерфейсе, сборка упадёт здесь,
// с понятным сообщением, а не где-то в глубине приложения.
//
// Приём стандартный для Go и стоит одну строку.
var _ sources.TrackSource = (*Source)(nil)

// Name — имя источника для журналов и базы.
func (s *Source) Name() string { return "file" }

// Ref — откуда именно брали треки.
func (s *Source) Ref() string { return s.path }

// Fetch читает файл и отдаёт треки.
func (s *Source) Fetch(ctx context.Context) ([]sources.RawTrack, error) {
	// Даже быстрая операция должна уважать отмену: если пользователь нажал
	// Ctrl+C до того, как мы успели открыть файл, начинать не нужно.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("открытие файла %s: %w", s.path, err)
	}
	defer f.Close()

	// strings.ToLower нужен, потому что на Windows файл вполне может
	// называться TRACKS.JSON — и это тот же самый формат.
	switch ext := strings.ToLower(filepath.Ext(s.path)); ext {
	case ".json":
		return parseJSON(f)
	case ".csv", ".txt":
		return parseCSV(f)
	default:
		return nil, fmt.Errorf(
			"не знаю, что делать с расширением %q: умею .json и .csv", ext)
	}
}

// ---------------------------------------------------------------------------
// JSON
// ---------------------------------------------------------------------------

// jsonTrack — один трек в файле JSON.
//
// Теги перечисляют, как поле называется в файле. Пакет encoding/json
// сопоставляет имена без учёта регистра, поэтому "Artist" и "artist"
// оба попадут в поле Artist.
type jsonTrack struct {
	Artist   string `json:"artist"`
	Title    string `json:"title"`
	Duration int    `json:"duration"`
	ID       string `json:"id"`
}

// jsonFile — файл целиком, если он обёрнут в объект.
type jsonFile struct {
	Tracks []jsonTrack `json:"tracks"`
}

// parseJSON разбирает файл JSON.
//
// Принимаем две формы записи:
//
//	{"tracks": [ {...}, {...} ]}   — объект с полем tracks
//	[ {...}, {...} ]               — просто массив
//
// Вторая нужна потому, что скрипты выгрузки из браузера обычно отдают
// голый массив, и заставлять человека руками оборачивать его в объект —
// лишний шаг, на котором он ошибётся.
func parseJSON(r io.Reader) ([]sources.RawTrack, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("чтение файла: %w", err)
	}

	// Смотрим на первый непробельный символ: квадратная скобка означает
	// массив, фигурная — объект. Дешевле, чем пытаться разобрать дважды.
	trimmed := strings.TrimLeft(string(raw), " \t\r\n\uFEFF")
	if trimmed == "" {
		return nil, fmt.Errorf("файл пустой")
	}

	var items []jsonTrack

	if trimmed[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("разбор массива JSON: %w", err)
		}
	} else {
		var wrapper jsonFile
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return nil, fmt.Errorf("разбор объекта JSON: %w", err)
		}
		items = wrapper.Tracks
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("в файле нет ни одного трека")
	}

	out := make([]sources.RawTrack, 0, len(items))
	for _, it := range items {
		// Треки без исполнителя И без названия пропускаем молча:
		// в выгрузках попадаются пустые строки, это не повод падать.
		if strings.TrimSpace(it.Artist) == "" && strings.TrimSpace(it.Title) == "" {
			continue
		}
		out = append(out, sources.RawTrack{
			Artist:      it.Artist,
			Title:       it.Title,
			DurationSec: it.Duration,
			SourceID:    it.ID,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// CSV
// ---------------------------------------------------------------------------

// parseCSV разбирает файл CSV.
//
// Ожидается первая строка с заголовками. Порядок колонок значения не имеет,
// колонки ищутся по имени — так файл, сохранённый из чужой таблицы, скорее
// подойдёт без правки.
//
// Понимаем такие имена колонок:
//
//	artist, исполнитель, автор
//	title, название, трек, песня
//	duration, длительность, время
//	id, source_id
func parseCSV(r io.Reader) ([]sources.RawTrack, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("чтение файла: %w", err)
	}
	text := strings.TrimPrefix(string(raw), "\uFEFF") // BOM, если файл из Excel

	reader := csv.NewReader(strings.NewReader(text))

	// Русский Excel сохраняет CSV с точкой с запятой вместо запятой,
	// потому что запятая у нас разделитель дробной части. Определяем
	// разделитель по первой строке: чего больше, то и разделитель.
	firstLine, _, _ := strings.Cut(text, "\n")
	if strings.Count(firstLine, ";") > strings.Count(firstLine, ",") {
		reader.Comma = ';'
	}

	// Строки с разным количеством полей не считаем ошибкой: в выгрузках
	// это обычное дело, а падать из-за одной кривой строки глупо.
	reader.FieldsPerRecord = -1

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("разбор CSV: %w", err)
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("в файле нет строк с данными (нужны заголовок и хотя бы один трек)")
	}

	// Раскладываем заголовки: имя колонки → её номер.
	index := map[string]int{}
	for i, name := range records[0] {
		index[strings.ToLower(strings.TrimSpace(name))] = i
	}

	artistCol := findColumn(index, "artist", "исполнитель", "автор")
	titleCol := findColumn(index, "title", "название", "трек", "песня")
	durCol := findColumn(index, "duration", "duration_sec", "длительность", "время")
	idCol := findColumn(index, "id", "source_id", "идентификатор")

	if artistCol < 0 || titleCol < 0 {
		return nil, fmt.Errorf(
			"в заголовке CSV не нашлись колонки исполнителя и названия; есть только: %s",
			strings.Join(records[0], ", "))
	}

	out := make([]sources.RawTrack, 0, len(records)-1)
	for _, row := range records[1:] {
		t := sources.RawTrack{
			Artist:   cell(row, artistCol),
			Title:    cell(row, titleCol),
			SourceID: cell(row, idCol),
			// Длительность может быть числом секунд или временем вида "4:04".
			DurationSec: parseDuration(cell(row, durCol)),
		}

		if strings.TrimSpace(t.Artist) == "" && strings.TrimSpace(t.Title) == "" {
			continue
		}
		out = append(out, t)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("в файле нет ни одного трека")
	}
	return out, nil
}

// findColumn ищет номер колонки по любому из перечисленных имён.
// Возвращает -1, если ни одно не подошло.
func findColumn(index map[string]int, names ...string) int {
	for _, n := range names {
		if i, ok := index[n]; ok {
			return i
		}
	}
	return -1
}

// cell безопасно достаёт ячейку строки. Если колонки нет или строка
// короче — возвращает пустоту вместо паники.
func cell(row []string, col int) string {
	if col < 0 || col >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[col])
}

// parseDuration понимает и "244", и "4:04", и "1:02:33".
func parseDuration(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	// Простое число секунд.
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}

	// Время с двоеточиями. Идём с конца: последняя часть всегда секунды,
	// предыдущая — минуты, ещё одна — часы. Так работает и для "4:04",
	// и для "1:02:33", без отдельных веток на каждый случай.
	parts := strings.Split(s, ":")
	total := 0
	multiplier := 1
	for i := len(parts) - 1; i >= 0; i-- {
		n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err != nil {
			return 0
		}
		total += n * multiplier
		multiplier *= 60
	}
	return total
}
