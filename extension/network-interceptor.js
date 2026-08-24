// network-interceptor.js — выполняется в "MAIN world", то есть в том же
// JavaScript-окружении, что и сама страница ВК, а не в изолированном мире
// обычного content script'а. Это принципиально: чтобы подсмотреть ответы
// fetch()/XMLHttpRequest, которые делает САМА страница, нужно подменить
// window.fetch и XMLHttpRequest.prototype ДО того, как код ВК успеет
// сохранить у себя ссылку на оригинал, — а обычный content script в
// изолированном мире видит другой, отдельный объект window и до
// оригинальных функций страницы просто не дотянется.
//
// "run_at": "document_start" в manifest.json обеспечивает, что этот файл
// выполнится раньше, чем браузер начнёт разбирать <body> страницы и её
// собственные <script>-теги — то есть раньше, чем код ВК вообще успеет
// прочитать window.fetch.
//
// Почему это вообще безопасно и не похоже на автоматизацию (см. Р-016
// в docs/DECISIONS.md): это не отдельный процесс браузера и не команда
// снаружи вкладки — это обычный код расширения, выполняющийся внутри
// той же самой, настоящей, уже залогиненной вкладки пользователя. Ни ВК,
// ни сам браузер не видят здесь ничего, отличimого от кода любого другого
// расширения (блокировщика рекламы, переводчика и так далее).
//
// Идея разбора ответов дословно перенесена из бывшего
// internal/sources/vk/vk.go (Go, версия до Р-016): не собирать запросы
// к web.api.vk.ru вручную, а слушать НАСТОЯЩИЕ ответы, которые получает
// сама страница, и рекурсивно искать в них массив под ключом "audios" —
// вне зависимости от того, в какую обёртку ВК его в этот раз завернул.

(function () {
	'use strict';

	// Домен внутреннего API ВК, на который приходят catalog.getAudio
	// и catalog.getSection (см. docs/VK_ACCESS.md). Слушаем весь этот
	// путь, а не конкретный метод — список того, что ВК заворачивает
	// в batch.call, нигде не документирован и может измениться.
	const API_PATH_MARKER = 'web.api.vk.ru/method/';

	// Имя канала для window.postMessage. Отдельная строка-маркер нужна,
	// чтобы content-ui.js (слушающий эти сообщения в изолированном мире)
	// мог отличить "наше" сообщение от любых других, которые страница
	// ВК и так гоняет через postMessage по своим собственным причинам.
	const MESSAGE_SOURCE = 'mgf-vk-extension';

	// collectAudiosArrays — рекурсивный обход разобранного JSON.
	//
	// Ответы внутреннего API ВК заворачивают полезные данные в разные
	// обёртки в зависимости от метода — вместо того чтобы разбирать
	// каждую обёртку по отдельности (и тем самым зависеть от того, что
	// она не поменяется), просто ищем массив с ключом "audios" на любой
	// глубине. Это то же самое послабление, ради которого был выбран
	// такой подход ещё в Р-014 (docs/DECISIONS.md): не завязываться на
	// точный контракт, а брать то, что похоже на список треков, где бы
	// оно ни лежало внутри ответа.
	function collectAudiosArrays(node, found) {
		if (Array.isArray(node)) {
			for (const child of node) collectAudiosArrays(child, found);
			return;
		}
		if (node && typeof node === 'object') {
			if (Array.isArray(node.audios)) {
				for (const item of node.audios) {
					if (item && typeof item === 'object') found.push(item);
				}
			}
			for (const key in node) {
				collectAudiosArrays(node[key], found);
			}
		}
	}

	// trackFromObject — превращает один разобранный объект трека ВК
	// в плоский объект вида {artist, title, duration, id}, ровно в том
	// формате, который уже понимает "music import file" (internal/sources/file
	// в Go-коде, тип jsonTrack). Так расширению не нужно ничего знать
	// про базу данных — оно просто говорит на языке, который уже умеет
	// разбирать существующий импортёр.
	//
	// Возвращает null, если в объекте нет ни artist, ни title — это
	// значит, что под ключом "audios" на самом деле лежало что-то другое
	// (в ответах ВК встречаются массивы с похожими именами не только
	// у треков), а не тихая порча данных.
	function trackFromObject(m) {
		const artist = typeof m.artist === 'string' ? m.artist : '';
		const title = typeof m.title === 'string' ? m.title : '';
		if (!artist.trim() && !title.trim()) return null;

		const duration = typeof m.duration === 'number' ? Math.round(m.duration) : 0;

		// id собираем в привычном для ВК виде "owner_id_id" — так треки
		// ссылаются друг на друга и в плейлистах внутри самого ответа ВК,
		// и этого достаточно, чтобы при повторной выгрузке узнать тот же
		// трек (дедупликация в самой базе тоже работает по этому полю).
		const trackID = m.id != null ? String(m.id) : '';
		const ownerID = m.owner_id != null ? String(m.owner_id) : '';
		const id = ownerID && trackID ? ownerID + '_' + trackID : trackID;

		return { artist, title, duration, id };
	}

	// handleResponseText — разбирает одно тело ответа и, если в нём
	// нашлись треки, отправляет их content-ui.js через postMessage.
	//
	// postMessage — единственный способ передать данные из MAIN-мира
	// в ISOLATED-мир content script'а: у них разные JS-окружения (разные
	// глобальные объекты, разные копии встроенных типов), но одно и то же
	// DOM-окно, а postMessage как раз и работает через окно, а не через
	// прямой вызов функций.
	function handleResponseText(text) {
		let parsed;
		try {
			parsed = JSON.parse(text);
		} catch (e) {
			return; // не JSON — не наш ответ, тихо пропускаем
		}

		const found = [];
		collectAudiosArrays(parsed, found);
		if (found.length === 0) return;

		const tracks = [];
		for (const item of found) {
			const track = trackFromObject(item);
			if (track) tracks.push(track);
		}
		if (tracks.length === 0) return;

		window.postMessage({ source: MESSAGE_SOURCE, type: 'tracks-batch', tracks }, '*');
	}

	// --- Подмена fetch --------------------------------------------------
	//
	// SPA ВК почти наверняка ходит именно через fetch, а не через
	// устаревший XMLHttpRequest, но перехватываем оба способа — дешёвая
	// подстраховка на случай, если внутри используется библиотека,
	// которая выбирает способ сама.
	const originalFetch = window.fetch;
	window.fetch = function (...args) {
		const promise = originalFetch.apply(this, args);

		// URL может быть первым аргументом (строка) или объектом Request.
		let url = '';
		try {
			const first = args[0];
			url = typeof first === 'string' ? first : (first && first.url) || '';
		} catch (e) {
			// ничего не делаем — просто не будем подсматривать этот запрос
		}

		if (url.includes(API_PATH_MARKER)) {
			promise
				.then((response) => {
					// .clone() обязателен: тело ответа можно прочитать только
					// один раз, а его ещё должен получить настоящий код
					// страницы — мы не хотим ничего у него отбирать, только
					// подсматривать копию.
					response
						.clone()
						.text()
						.then(handleResponseText)
						.catch(() => {});
				})
				.catch(() => {});
		}

		return promise;
	};

	// --- Подмена XMLHttpRequest ------------------------------------------
	const OriginalXHR = window.XMLHttpRequest;
	const originalOpen = OriginalXHR.prototype.open;
	const originalSend = OriginalXHR.prototype.send;

	OriginalXHR.prototype.open = function (method, url, ...rest) {
		this.__mgfURL = url; // запоминаем адрес, чтобы проверить его в send
		return originalOpen.call(this, method, url, ...rest);
	};

	OriginalXHR.prototype.send = function (...args) {
		if (typeof this.__mgfURL === 'string' && this.__mgfURL.includes(API_PATH_MARKER)) {
			this.addEventListener('load', function () {
				try {
					handleResponseText(this.responseText);
				} catch (e) {
					// тело могло быть не текстом (например, responseType
					// уже установлен в "json" или "blob") — тогда просто
					// пропускаем, fetch-перехватчик выше и так покрывает
					// подавляющее большинство случаев.
				}
			});
		}
		return originalSend.apply(this, args);
	};
})();
