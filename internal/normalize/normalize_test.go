package normalize

import "testing"

// Файлы с тестами в Go лежат рядом с кодом и называются *_test.go.
// Компилятор их в обычную сборку не включает — они существуют только
// для команды "go test".
//
// Правила, которые видно на примерах ниже:
//   - функция теста называется TestЧтоТо и принимает *testing.T;
//   - t.Errorf сообщает о провале, но тест продолжает выполняться;
//   - t.Run создаёт подтест со своим именем — в отчёте будет видно,
//     какой именно случай сломался, а не просто "тест упал".
//
// Такой стиль называется табличным: список случаев отдельно, один
// прогон в цикле. Добавить новый случай — дописать строку в таблицу,
// а не копировать функцию целиком. Когда завтра встретится очередная
// кривая строка из ВК, её место здесь.

func TestArtist(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"обычное имя", "Nirvana", "nirvana"},
		{"верхний регистр", "NIRVANA", "nirvana"},
		{"лишние пробелы", "   Nirvana   ", "nirvana"},
		{"пробелы внутри", "Pink   Floyd", "pink floyd"},
		{"косая черта", "AC/DC", "ac dc"},
		{"точки", "will.i.am", "will i am"},
		{"кириллица", "Король и Шут", "король и шут"},
		{"ё приводится к е", "Ёлка", "елка"},
		{"й не ломается", "Мой Рок", "мой рок"},
		{"диакритика латиницы", "Björk", "bjork"},
		{"диакритика французская", "Édith Piaf", "edith piaf"},
		{"амперсанд не разделитель", "Simon & Garfunkel", "simon garfunkel"},
		{"участники отрезаются", "Miyagi feat. Andy Panda", "miyagi"},
		{"участники через ft.", "Eminem ft. Rihanna", "eminem"},
		{"участники по-русски", "Баста при участии Гуфа", "баста"},
		{"ft внутри слова не трогаем", "Daft Punk", "daft punk"},
		{"пустая строка", "", ""},
	}

	for _, c := range cases {
		// Переменная цикла копируется в локальную: до Go 1.22 без этого
		// все подтесты видели бы последнее значение. Сейчас язык это
		// исправил, но привычка полезная — код часто читают старые люди.
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := Artist(c.in)
			if got != c.want {
				t.Errorf("Artist(%q)\n получили: %q\n ожидали:  %q", c.in, got, c.want)
			}
		})
	}
}

func TestTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"обычное название", "Smells Like Teen Spirit", "smells like teen spirit"},
		{"official video", "Smells Like Teen Spirit (Official Video)", "smells like teen spirit"},
		{"квадратные скобки", "Территория [HD]", "территория"},
		{"ремастеринг с годом", "Come Together (Remastered 2009)", "come together"},
		{"голый год", "Believer (2017)", "believer"},
		{"хвост после дефиса", "Numb - Official Music Video", "numb"},
		{"хвост после черты", "Лирика | Клип", "лирика"},
		{"участники в названии", "Lose Yourself (feat. Someone)", "lose yourself"},
		{"участники без скобок", "Rap God ft. Nobody", "rap god"},
		{"содержательные скобки остаются", "(I Can't Get No) Satisfaction", "i can t get no satisfaction"},
		{"дефис внутри названия не трогаем", "Jack - The Ripper", "jack the ripper"},
		{"несколько мусорных групп", "Song (Official Video) [HD] (2020)", "song"},
		{"пустые скобки", "Song ()", "song"},
		{"адрес в скобках", "Песня (vk.com/music)", "песня"},
		{"регистр и пробелы", "  SMELLS   like  Teen SPIRIT ", "smells like teen spirit"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := Title(c.in)
			if got != c.want {
				t.Errorf("Title(%q)\n получили: %q\n ожидали:  %q", c.in, got, c.want)
			}
		})
	}
}

// TestОдинаковыеТрекиДаютОдинКлюч — главная проверка всего пакета.
//
// Остальные тесты проверяют детали. Этот проверяет смысл: четыре разных
// написания одной песни должны схлопнуться в один ключ. Если сломается
// он — сломалась дедупликация, и в базе заведутся дубли.
func TestОдинаковыеТрекиДаютОдинКлюч(t *testing.T) {
	variants := []struct{ artist, title string }{
		{"Nirvana", "Smells Like Teen Spirit"},
		{"NIRVANA", "Smells Like Teen Spirit (Official Video)"},
		{"  nirvana  ", "smells like teen spirit [HD]"},
		{"Nirvana feat. Unknown", "Smells Like Teen Spirit (Remastered 2011)"},
	}

	wantArtist := Artist(variants[0].artist)
	wantTitle := Title(variants[0].title)

	for _, v := range variants {
		if got := Artist(v.artist); got != wantArtist {
			t.Errorf("исполнитель %q дал ключ %q, а ожидался %q", v.artist, got, wantArtist)
		}
		if got := Title(v.title); got != wantTitle {
			t.Errorf("название %q дало ключ %q, а ожидался %q", v.title, got, wantTitle)
		}
	}
}

// TestРазныеТрекиНеСхлопываются — обратная проверка.
//
// Нормализация, которая склеивает всё подряд, тоже "работает": она даёт
// один ключ на всю библиотеку. Поэтому важно проверять не только то,
// что одинаковое совпало, но и что разное осталось разным.
func TestРазныеТрекиНеСхлопываются(t *testing.T) {
	pairs := [][2]string{
		{"Nirvana", "Nirvana UK"},
		{"Король и Шут", "Король"},
		{"Кино", "Кина"},
	}

	for _, p := range pairs {
		if Artist(p[0]) == Artist(p[1]) {
			t.Errorf("исполнители %q и %q схлопнулись в один ключ %q",
				p[0], p[1], Artist(p[0]))
		}
	}
}
