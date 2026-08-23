// Пакет normalize приводит грязные строки из источника к виду, по которому
// их можно сравнивать между собой и искать во внешних справочниках.
//
// Зачем это нужно. В ВКонтакте исполнителя и название набивали живые люди
// на протяжении пятнадцати лет. Одна и та же песня выглядит так:
//
//	Nirvana — Smells Like Teen Spirit
//	NIRVANA - Smells Like Teen Spirit (Official Video)
//	Nirvana  -  Smells like teen spirit [HD]
//	Nirvana feat. Unknown — Smells Like Teen Spirit (Remastered 2011)
//
// Для человека это очевидно один трек. Для программы — четыре разных строки.
// Задача пакета: получить из всех четырёх один и тот же ключ.
//
// Важное свойство: функции этого пакета — чистые. Они не ходят в базу,
// не лезут в сеть, не читают файлы. Один и тот же вход всегда даёт один
// и тот же выход. Именно поэтому их так удобно покрывать тестами: никакой
// подготовки окружения, просто вход и ожидаемый результат.
package normalize

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ---------------------------------------------------------------------------
// Публичные функции
// ---------------------------------------------------------------------------

// Artist приводит имя исполнителя к ключу для сравнения.
//
//	"  NIRVANA " → "nirvana"
//	"Король и Шут" → "король и шут"
//	"AC/DC" → "ac dc"
func Artist(raw string) string {
	return key(PrimaryArtist(raw))
}

// Title приводит название трека к ключу для сравнения.
//
//	"Smells Like Teen Spirit (Official Video)" → "smells like teen spirit"
//	"Территория [HD]" → "территория"
func Title(raw string) string {
	s := stripJunkBrackets(raw)
	s = stripFeaturing(s)
	s = stripJunkTail(s)
	return key(s)
}

// PrimaryArtist выделяет основного исполнителя из строки с участниками.
//
//	"Miyagi feat. Andy Panda" → "Miyagi"
//	"Баста при участии Гуфа"  → "Баста"
//
// Зачем это делается отдельно и почему именно так. Внешние справочники
// (MusicBrainz, Last.fm) знают исполнителя "Miyagi". Строку целиком,
// вместе с участниками, они не найдут — и трек останется без жанра.
// Поэтому для поиска берём основного исполнителя.
//
// Чем платим: совместный трек припишется только первому участнику.
// Для нашей задачи — отфильтровать библиотеку по жанру — это приемлемо.
// Если однажды окажется, что нет, участников можно будет складывать
// в artist_aliases и учитывать отдельно.
//
// Символ "&" сознательно НЕ считается разделителем: "Simon & Garfunkel"
// и "Mott & Bailey" — это цельные названия коллективов, а не два артиста.
func PrimaryArtist(raw string) string {
	s := strings.TrimSpace(raw)
	lower := strings.ToLower(s)

	// Ищем самый ранний разделитель участников и режем строку по нему.
	cut := len(s)
	for _, sep := range featuringSeparators {
		if i := strings.Index(lower, sep); i > 0 && i < cut {
			cut = i
		}
	}
	return strings.TrimSpace(s[:cut])
}

// ---------------------------------------------------------------------------
// Внутренняя кухня
// ---------------------------------------------------------------------------

// featuringSeparators — что считаем признаком "дальше идут участники".
//
// Все варианты в нижнем регистре и с пробелом впереди: без пробела
// подстрока "ft" нашлась бы внутри слова "Daft Punk" и отрезала бы
// половину названия. Такие мелочи и отличают работающую нормализацию
// от той, которая портит каждый десятый трек.
var featuringSeparators = []string{
	" feat.", " feat ", " ft.", " ft ", " featuring ",
	" при участии ", " при уч.", " совместно с ", " prod. by ", " prod.by ",
}

// junkWords — слова, по которым содержимое скобок считается мусором.
//
// Логика такая: скобки в названии трека чаще всего добавил не автор,
// а тот, кто заливал файл. "(Official Video)", "[HD]", "(Ремастеринг 2011)" —
// это про файл, а не про песню. Но бывают и содержательные скобки:
// "(I Can't Get No) Satisfaction" — часть настоящего названия.
//
// Поэтому мы не выкидываем скобки подряд, а смотрим внутрь: есть там
// слово из этого списка — выкидываем, нет — оставляем текст без скобок.
var junkWords = []string{
	"official", "video", "clip", "клип", "lyrics", "lyric", "текст",
	"audio", "hd", "hq", "4k", "full", "quality",
	"remaster", "remastered", "ремастер", "ремастеринг",
	"explicit", "clean", "radio edit", "radio version",
	"bonus", "бонус", "минус", "минусовка", "cover", "кавер",
	"премьера", "новинка", "новинки", "рингтон",
	"ost", "soundtrack", "саундтрек", "из фильма", "из сериала",
	"prod", "prod.", "feat", "feat.", "ft.",
	"vk.com", "vk.ru", "www", "http",
}

// bracketGroup находит содержимое круглых или квадратных скобок.
//
// Разбор выражения по частям:
//
//	[\(\[]     — открывающая круглая или квадратная скобка
//	([^\)\]]*) — что угодно, кроме закрывающих скобок; скобки вокруг
//	             делают это "группой захвата", её потом можно достать
//	[\)\]]     — закрывающая скобка
var bracketGroup = regexp.MustCompile(`[\(\[]([^\)\]]*)[\)\]]`)

// yearOnly — год в скобках и ничего больше: "(2019)".
var yearOnly = regexp.MustCompile(`^\s*(19|20)\d{2}\s*$`)

// stripJunkBrackets убирает скобки с мусором, оставляя содержательные.
func stripJunkBrackets(s string) string {
	return bracketGroup.ReplaceAllStringFunc(s, func(match string) string {
		// Достаём содержимое без самих скобок.
		inner := strings.ToLower(strings.Trim(match, "()[]"))

		// Пустые скобки — мусор по определению.
		if strings.TrimSpace(inner) == "" {
			return ""
		}
		// Голый год — тоже мусор.
		if yearOnly.MatchString(inner) {
			return ""
		}
		// Есть внутри мусорное слово — выкидываем всю группу.
		for _, w := range junkWords {
			if strings.Contains(inner, w) {
				return ""
			}
		}
		// Содержательные скобки оставляем, но сами скобки убираем:
		// они мешают сравнению, а текст внутри нужен.
		return " " + strings.Trim(match, "()[]") + " "
	})
}

// stripFeaturing убирает хвост с участниками из названия трека.
func stripFeaturing(s string) string {
	lower := strings.ToLower(s)
	cut := len(s)
	for _, sep := range featuringSeparators {
		if i := strings.Index(lower, sep); i > 0 && i < cut {
			cut = i
		}
	}
	return strings.TrimSpace(s[:cut])
}

// junkTail — хвост после дефиса или вертикальной черты: "Песня - Official Video".
var junkTail = regexp.MustCompile(`\s+[-–—|]\s+([^-–—|]+)$`)

// stripJunkTail убирает хвост после разделителя, если в нём мусорное слово.
//
// Осторожность здесь не лишняя: " - " встречается и в настоящих названиях
// ("Sunday Bloody Sunday - Live from Boston" — спорно, а вот
// "Jack - The Ripper" уже часть имени). Поэтому режем только тогда,
// когда в хвосте есть слово из списка мусора.
func stripJunkTail(s string) string {
	m := junkTail.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	tail := strings.ToLower(m[1])
	for _, w := range junkWords {
		if strings.Contains(tail, w) {
			return strings.TrimSpace(s[:len(s)-len(m[0])])
		}
	}
	return s
}

// key — общая часть нормализации: регистр, юникод, пунктуация, пробелы.
func key(s string) string {
	s = stripLatinMarks(s)

	// ё и е люди пишут как попало, и это одна и та же буква на слух.
	// Делается ПОСЛЕ снятия диакритики: иначе NFC вернул бы ё обратно.
	s = strings.NewReplacer("ё", "е", "Ё", "Е").Replace(s)

	s = strings.ToLower(s)

	// Пунктуацию заменяем пробелом, а не удаляем.
	// Разница существенная: "AC/DC" при удалении дал бы "acdc",
	// а при замене — "ac dc". Второе ближе к тому, как это ищут люди
	// и как это записано во внешних справочниках.
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}

	// strings.Fields разбивает по любым пробельным символам и сам
	// выбрасывает пустые куски. Join обратно через один пробел —
	// получается схлопывание любого количества пробелов в один.
	return strings.Join(strings.Fields(b.String()), " ")
}

// stripLatinMarks снимает диакритику с латиницы: é → e, ü → u, ñ → n.
//
// Здесь есть тонкость, из-за которой функция сложнее, чем могла бы быть.
//
// Стандартный приём — разложить строку по форме NFD (буква отдельно,
// значок отдельно) и выбросить все значки. Для латиницы работает отлично.
// Но кириллица от этого страдает: буква "й" в NFD разбирается на "и" плюс
// краткая, и после выбрасывания значков "мой" превращается в "мои".
// А "ё" — в "е", что нам как раз нужно, но по другой причине.
//
// Поэтому значки выбрасываются только у латинских букв. У кириллических
// они остаются, и обратная сборка через NFC возвращает "й" на место.
func stripLatinMarks(s string) string {
	var b strings.Builder
	var lastBase rune

	for _, r := range norm.NFD.String(s) {
		// unicode.Mn — категория "Mark, nonspacing": те самые значки
		// над и под буквами, которые в NFD отделяются от основы.
		if unicode.Is(unicode.Mn, r) {
			if unicode.Is(unicode.Cyrillic, lastBase) {
				b.WriteRune(r) // кириллице значок возвращаем
			}
			continue // латинице — нет
		}
		lastBase = r
		b.WriteRune(r)
	}

	// NFC собирает буквы со значками обратно в единые символы.
	return norm.NFC.String(b.String())
}
