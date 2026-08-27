// content-ui.js — обычный content script (изолированный мир, значение
// по умолчанию для manifest v3, поэтому в manifest.json у него нет поля
// "world"). Отвечает за три вещи: показать человеку маленькую панель
// управления прямо на странице, докрутить список треков до конца и
// собрать в файл то, что нашёл network-interceptor.js (он работает в
// MAIN-мире и сам ничего скачать не может — у него нет доступа
// к DOM-элементам вроде <a download>, которые здесь и создаются).
//
// Общая идея и даже эвристика прокрутки — прямое продолжение того, что
// раньше делал internal/sources/vk/vk.go на Go через chromedp (до Р-016
// в docs/DECISIONS.md). Разница только в том, ГДЕ это выполняется:
// раньше — снаружи, управляя браузером по протоколу DevTools, теперь —
// изнутри самой вкладки, обычным кодом расширения.

(function () {
	'use strict';

	const MESSAGE_SOURCE = 'mgf-vk-extension';

	// IDLE_ROUNDS_TO_STOP — сколько подряд прокруток без единого нового
	// трека считать признаком "долистали до конца". Одной прокрутки без
	// результата мало: сервер мог просто не успеть ответить. В vk.go
	// (до Р-016) здесь стояло 4 — но на первом же реальном прогоне (1898
	// из 1918 треков Рус-Лана, на 20 меньше) стало видно, что этого
	// иногда не хватает: ближе к концу длинного списка сервер отвечает
	// медленнее, и одна "медленная" порция может не успеть прийти за
	// 4 раунда. Увеличено с запасом — цена ошибки в другую сторону
	// (подождать на 4 секунды дольше в конце) намного дешевле, чем
	// молча потерять последнюю порцию треков.
	const IDLE_ROUNDS_TO_STOP = 6;

	// SCROLL_DELAY_MS — пауза между прокрутками. Смысл тот же, что и был
	// в VK_REQUEST_DELAY_SECONDS (Р-013, docs/DECISIONS.md): не пауза
	// ради паузы, а то, что отличает "человек листает страницу" от "бот
	// долбит сервер". Но здесь это соображение применяется по-другому,
	// чем раньше в vk.go, — и это стоит явно проговорить, а не просто
	// скопировать старое число. В Р-013/Р-014 приложение само слало HTTP-
	// запросы к серверу ВК, и пауза защищала СЕРВЕР от нагрузки, похожей
	// на бота. Здесь же (Р-016) ни один запрос не создаётся расширением —
	// оно только прокручивает DOM настоящей вкладки, а все запросы к ВК
	// делает сама страница, точно так же, как делала бы при обычной
	// прокрутке живым человеком. Поэтому пауза короче старой: 1.2 секунды
	// вместо 2 — она не столько "маскировка", сколько практический
	// компромисс между скоростью и тем, чтобы очередной запрос страницы
	// успел прийти и попасть в счётчик до следующей прокрутки.
	const SCROLL_DELAY_MS = 1200;

	// MAX_SCROLL_ROUNDS — тот же предохранитель, что maxScrollRounds
	// в vk.go: жёсткий потолок на случай, если эвристика прокрутки
	// почему-то никогда не находит "конец списка".
	const MAX_SCROLL_ROUNDS = 500;

	// tracksByID — собранные треки, ключ — тот самый id вида
	// "owner_id_id", который формирует network-interceptor.js. Один и
	// тот же трек вполне может прийти повторно (например, если разные
	// прокрутки частично пересеклись) — Map по ключу избавляет от
	// повторов без сравнения строк, ровно как bodyCollector в vk.go.
	const tracksByID = new Map();

	let collecting = false;
	let scrollTimer = null;

	// hasRun — отличает "ещё ни разу не нажимал 'Начать сбор', но кое-что
	// уже само поймалось" от "нажимал, останавливал". Нужно только для
	// текста статуса на панели (см. updatePanel) — само поведение сбора
	// от него не зависит.
	let hasRun = false;

	// expectedTotal — сколько треков, по словам самой страницы ВК, всего
	// должно быть. Кэшируется один раз, найденной (см. readExpectedTotal):
	// само число за время одного прохода не меняется, а искать его в тексте
	// всей страницы на каждой прокрутке — лишняя работа без пользы.
	let expectedTotal = null;

	// readExpectedTotal ищет в тексте страницы фразу вида "1 918 треков ·
	// 17 плейлистов" — так ВК подписывает профиль над списком. Это только
	// для ОТОБРАЖЕНИЯ прогресса человеку (честное "217 из 1918"), а не для
	// решения, когда останавливаться, — останавливает по-прежнему
	// IDLE_ROUNDS_TO_STOP: если это число окажется неверным, устаревшим
	// или вообще не найдётся (у страницы своих треков, например, заголовок
	// может выглядеть иначе — см. скриншот "Мои треки"), сбор всё равно
	// доведёт дело до конца сам, просто без цифры "из скольки".
	//
	// Специально ищем связку "N треков · M плейлистов" вместе, а не одно
	// число рядом со словом "треков" — иначе есть риск случайно зацепить
	// подпись отдельного плейлиста ниже на той же странице.
	function readExpectedTotal() {
		const text = document.body.innerText || '';
		const match = text.match(/(\d[\d\s ]{0,10})\s*треков?\s*[·•]\s*\d+\s*плейлист/i);
		if (!match) return null;
		const digits = match[1].replace(/[\s ]/g, '');
		const n = parseInt(digits, 10);
		return Number.isFinite(n) && n > 0 ? n : null;
	}

	// --- Приём треков от network-interceptor.js --------------------------
	//
	// window.addEventListener здесь — не тот же window, что видит
	// network-interceptor.js (у content script'а в изолированном мире
	// свой JS-контекст), но DOM-окно одно на всех, и postMessage как раз
	// и придуман для обмена между разными мирами одной и той же страницы.
	//
	// Важно: этот слушатель ничего не проверяет насчёт "идёт ли сейчас
	// сбор" — он принимает треки всегда, даже если "Начать сбор" ещё
	// ни разу не нажимали. Это осознанно, а не недосмотр: сама страница
	// ВК и без нас подгружает первую порцию треков сразу при открытии
	// (то, что видно в начале списка без единой прокрутки), и эти
	// сетевые ответы так же реальны, как и все остальные — не ловить их
	// значило бы терять готовые данные на ровном месте. Из-за этого сразу
	// после открытия страницы счётчик может показывать не 0, а размер
	// первой порции (обычно около 20) — это не баг, а честный подсчёт
	// того, что страница уже успела показать сама (см. updatePanel).
	window.addEventListener('message', (event) => {
		// event.source === window проверяет, что сообщение пришло из
		// этого же окна (а не из вложенного iframe постороннего сайта) —
		// стандартная предосторожность для postMessage с получателем '*'.
		if (event.source !== window) return;
		const data = event.data;
		if (!data || data.source !== MESSAGE_SOURCE || data.type !== 'tracks-batch') return;

		for (const track of data.tracks) {
			if (track.id) {
				tracksByID.set(track.id, track);
			} else {
				// Трек без id (бывает у части ответов) всё равно стоит
				// сохранить — просто под собственным синтетическим
				// ключом, чтобы не потерять и не схлопнуть с другим.
				tracksByID.set('no-id-' + tracksByID.size, track);
			}
		}

		updatePanel();
	});

	// --- Эвристика прокрутки ----------------------------------------------
	//
	// Дословно перенесено из scrollJS в vk.go: ищем на странице самый
	// длинный прокручиваемый элемент (в SPA ВК список треков почти
	// всегда лежит не во всей странице, а во внутреннем блоке со своей
	// прокруткой) — это осознанная эвристика, а не разбор вёрстки ВК:
	// имена CSS-классов внутри SPA нигде не документированы и могут
	// поменяться в любой момент, а "самый длинный прокручиваемый блок" —
	// то, что остаётся верным независимо от конкретных названий классов.
	function findScrollContainer() {
		let best = null;
		const all = document.querySelectorAll('*');
		for (const el of all) {
			if (el.scrollHeight > el.clientHeight + 50) {
				if (!best || el.scrollHeight > best.scrollHeight) {
					best = el;
				}
			}
		}
		return best;
	}

	// scrollOnce докручивает и окно, и найденный внутренний блок до конца
	// (на случай, если список всё-таки обычный, а не во вложенном блоке).
	// Расстояние за один раз — с запасом (innerHeight * 6, не * 3, как
	// было раньше): страница ВК всё равно подгружает данные порциями по
	// своему усмотрению, и более длинный прыжок просто быстрее упирается
	// в её собственный порог подгрузки следующей порции — не пропускает
	// ничего, потому что финальный best.scrollTop = best.scrollHeight
	// в любом случае докручивает блок до самого низа.
	function scrollOnce() {
		window.scrollBy(0, window.innerHeight * 6);
		const best = findScrollContainer();
		if (best) {
			best.scrollTop = best.scrollHeight;
		}
	}

	// scrollToTop — используется перед началом нового прохода (см.
	// startCollecting). Сама по себе прокрутка наверх не нужна для
	// полноты данных: сетевой перехватчик ловит ответы всегда, независимо
	// от того, где сейчас находится видимая часть списка (см. комментарий
	// у window.addEventListener('message', ...) выше). Но пользователю
	// нужен предсказуемый, видимый проход "сверху вниз", а не продолжение
	// от случайного места, на которое он мог провернуть колесо мыши сам.
	function scrollToTop() {
		window.scrollTo(0, 0);
		const best = findScrollContainer();
		if (best) {
			best.scrollTop = 0;
		}
	}

	// --- Основной цикл сбора ----------------------------------------------
	//
	// Важно: запуск НЕ очищает то, что уже собрано раньше (не было так
	// в первой версии — из-за этого повторный "Начать сбор" стирал уже
	// найденные треки, хотя было бы полезно наоборот — дособрать
	// недостающий хвост списка, если счётчик остановился раньше, чем
	// показывает сама страница ВК). Явный сброс — отдельная кнопка
	// "Сбросить" (см. resetTracks), а не побочный эффект запуска.
	// Это же и позволяет осознанно объединить несколько страниц в один
	// файл (например, свою библиотеку и библиотеку друга подряд, без
	// сброса между ними) — само по себе полезное поведение, а не только
	// подстраховка от потери данных.
	function startCollecting() {
		if (collecting) return;
		collecting = true;
		hasRun = true;
		scrollToTop();
		updatePanel();

		let round = 0;
		let idleRounds = 0;
		let lastCount = tracksByID.size;

		scrollTimer = setInterval(() => {
			round++;
			scrollOnce();

			const count = tracksByID.size;
			if (count === lastCount) {
				// Не считаем раунды простоя, пока не пришёл хотя бы один
				// трек — первый ответ может приехать не сразу, странице
				// нужно время скачать и выполнить весь JS. Ровно та же
				// причина, по которой в vk.go эта же проверка стоит
				// именно так (см. комментарий там про "гарантированно
				// сдастся раньше времени").
				if (count > 0) idleRounds++;
			} else {
				idleRounds = 0;
				lastCount = count;
			}

			updatePanel();

			if (idleRounds >= IDLE_ROUNDS_TO_STOP || round >= MAX_SCROLL_ROUNDS) {
				stopCollecting();
			}
		}, SCROLL_DELAY_MS);
	}

	function stopCollecting() {
		if (!collecting) return;
		collecting = false;
		clearInterval(scrollTimer);
		scrollTimer = null;
		updatePanel();
	}

	// resetTracks — явная очистка счётчика по кнопке. Останавливает сбор,
	// если он ещё идёт (нет смысла продолжать прокрутку в фон, если
	// результат всё равно сейчас обнулится), и возвращает панель в самое
	// первое состояние.
	function resetTracks() {
		stopCollecting();
		tracksByID.clear();
		hasRun = false;
		// expectedTotal тоже сбрасываем: чаще всего "Сбросить" нажимают
		// именно потому, что перешли на страницу другого человека, и старое
		// закэшированное число там уже неверно. Следующее updatePanel
		// прочитает его заново с текущей страницы.
		expectedTotal = null;
		updatePanel();
	}

	// --- Скачивание результата ---------------------------------------------
	//
	// Формат — ровно тот, что понимает internal/sources/file (Go): либо
	// голый массив объектов, либо {"tracks":[...]}. Берём вторую форму
	// как более явную. Дальше человек запускает обычный
	// "music import file tracks.json --library ..." — никакой другой
	// связи между расширением и Go-программой нет и не нужно (Р-016).
	function downloadJSON() {
		const tracks = Array.from(tracksByID.values());
		const payload = JSON.stringify({ tracks }, null, 2);
		const blob = new Blob([payload], { type: 'application/json' });
		const url = URL.createObjectURL(blob);

		const a = document.createElement('a');
		a.href = url;
		a.download = 'tracks.json';
		document.body.appendChild(a);
		a.click();
		a.remove();

		// Освобождаем URL не сразу, а с небольшой задержкой: некоторые
		// браузеры запускают скачивание асинхронно, и слишком ранний
		// revokeObjectURL иногда обрывает его на середине.
		setTimeout(() => URL.revokeObjectURL(url), 5000);
	}

	// --- Панель на странице -------------------------------------------------
	//
	// Простая плавающая панель поверх страницы ВК — без разбора вёрстки
	// самого ВК, чтобы её не сломало ни одно их обновление интерфейса.
	let panel, statusEl, countEl, startBtn, downloadBtn, resetLink;

	function buildPanel() {
		panel = document.createElement('div');
		panel.style.cssText = [
			'position:fixed', 'z-index:2147483647', 'right:16px', 'bottom:16px',
			'background:#1b1e24', 'color:#fff', 'font:14px/1.4 sans-serif',
			'padding:12px 14px', 'border-radius:10px', 'box-shadow:0 4px 16px rgba(0,0,0,.4)',
			'display:flex', 'flex-direction:column', 'gap:8px', 'min-width:220px',
		].join(';');

		const title = document.createElement('div');
		title.textContent = 'Tulle Music Genre Filter — сбор треков';
		title.style.cssText = 'font-weight:600';

		statusEl = document.createElement('div');
		countEl = document.createElement('div');

		startBtn = document.createElement('button');
		downloadBtn = document.createElement('button');
		for (const btn of [startBtn, downloadBtn]) {
			btn.style.cssText = [
				'padding:6px 10px', 'border:none', 'border-radius:6px',
				'cursor:pointer', 'font:inherit',
			].join(';');
		}
		startBtn.style.background = '#4c8bf5';
		startBtn.style.color = '#fff';
		downloadBtn.style.background = '#2fa84f';
		downloadBtn.style.color = '#fff';
		// Текст кнопки. Раньше здесь ничего не было — updatePanel() задавал
		// текст только у startBtn, а про downloadBtn забыл: кнопка была
		// видна как сплошной зелёный прямоугольник без единой буквы (то
		// самое "зелёная кнопочка не используется"). Само нажатие при этом
		// работало — не хватало только подписи.
		downloadBtn.textContent = 'Скачать JSON';

		startBtn.addEventListener('click', () => {
			if (collecting) {
				stopCollecting();
			} else {
				startCollecting();
			}
		});
		downloadBtn.addEventListener('click', downloadJSON);

		// resetLink — маленькая текстовая ссылка, а не полноразмерная
		// кнопка: сброс — редкое, "опасное" действие (стирает уже
		// найденное), ей не место наравне по весу с "Начать сбор" и
		// "Скачать". Подтверждения нарочно нет — данные всё равно уже
		// либо скачаны, либо легко пересобираются повторным проходом.
		resetLink = document.createElement('a');
		resetLink.textContent = 'Сбросить счётчик';
		resetLink.href = '#';
		resetLink.style.cssText = 'color:#9aa4b2; font-size:12px; text-align:right; cursor:pointer;';
		resetLink.addEventListener('click', (e) => {
			e.preventDefault();
			resetTracks();
		});

		panel.append(title, statusEl, countEl, startBtn, downloadBtn, resetLink);
		document.body.appendChild(panel);
		updatePanel();
	}

	function updatePanel() {
		if (!panel) return;
		const count = tracksByID.size;

		if (collecting) {
			statusEl.textContent = 'Собираю... (прокручиваю страницу)';
		} else if (hasRun) {
			statusEl.textContent = 'Остановлено';
		} else if (count > 0) {
			// Ничего ещё не нажимали, но счётчик не ноль — это не баг:
			// страница ВК сама подгружает первую порцию треков при
			// открытии, и перехватчик честно её посчитал. Проговариваем
			// это явно, чтобы не выглядело как "само что-то запустилось".
			statusEl.textContent = 'Готово к запуску (кое-что уже поймано само)';
		} else {
			statusEl.textContent = 'Готово к запуску';
		}

		if (expectedTotal === null) {
			expectedTotal = readExpectedTotal();
		}
		countEl.textContent = expectedTotal
			? 'Найдено треков: ' + count + ' из ' + expectedTotal
			: 'Найдено треков: ' + count;

		startBtn.textContent = collecting ? 'Остановить' : 'Начать сбор';
		downloadBtn.disabled = count === 0;
		downloadBtn.style.opacity = count === 0 ? '0.5' : '1';
	}

	// document_idle гарантирует, что document.body уже существует —
	// на document_start (как у network-interceptor.js) его могло ещё
	// не быть.
	buildPanel();
})();
