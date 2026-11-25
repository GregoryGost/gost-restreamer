package api

import (
	"embed"
	"net/http"
	"strconv"
	"strings"
)

// chatWeb содержит страницы, клиентские модули, стили и значки площадок.
//
//go:embed chatweb/*.html chatweb/assets/*.js chatweb/assets/*.css chatweb/assets/*.svg
var chatWeb embed.FS

// registerChatWeb выдаёт две самостоятельные страницы с настроенными пределами ленты и регистрирует
// только встроенные клиентские ресурсы. Ошибка чтения прерывает создание API через registrationErr.
func (a *API) registerChatWeb() {
	// page указывает очередной вид ленты, limit задаёт его серверный предел сообщений в DOM.
	for page, limit := range map[string]int{
		"overlay": a.config.ChatLimits.OverlayMessages, "dashboard": a.config.ChatLimits.DashboardMessages,
	} {
		// document содержит исходную страницу, err сообщает об ошибке чтения из встроенной поставки.
		document, err := chatWeb.ReadFile("chatweb/" + page + ".html")
		if err != nil {
			a.registrationErr = err
			return
		}
		a.router.Get("/chat/"+page, serveChatWeb("text/html; charset=utf-8", []byte(strings.Replace(string(document), "{{messageLimit}}", strconv.Itoa(limit), 1))))
	}
	// entries содержит только файлы встроенного каталога ресурсов, err сообщает об ошибке его чтения.
	entries, err := chatWeb.ReadDir("chatweb/assets")
	if err != nil {
		a.registrationErr = err
		return
	}
	// entry определяет имя очередного встроенного ресурса, по которому регистрируется точный маршрут.
	for _, entry := range entries {
		// filename содержит имя ресурса, а contentType задаёт его фактический формат для браузера.
		filename, contentType := entry.Name(), "text/javascript; charset=utf-8"
		switch {
		case strings.HasSuffix(filename, ".css"):
			contentType = "text/css; charset=utf-8"
		case strings.HasSuffix(filename, ".svg"):
			contentType = "image/svg+xml"
		}
		// asset содержит байты ресурса, err сообщает об ошибке внутри встроенной поставки.
		asset, err := chatWeb.ReadFile("chatweb/assets/" + filename)
		if err != nil {
			a.registrationErr = err
			return
		}
		a.router.Get("/chat/assets/"+filename, serveChatWeb(contentType, asset))
	}
}

// serveChatWeb возвращает обработчик неизменяемых байтов в заданном формате. Политика страницы
// разрешает загрузить модули и читать ленту на этом сервере, а также показывать встроенные
// значки и внешние HTTPS-изображения.
func serveChatWeb(contentType string, data []byte) http.HandlerFunc {
	// Обработчик выдаёт заранее подготовленный ресурс без использования данных запроса в HTML.
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; img-src 'self' https:; script-src 'self'; style-src 'self'; base-uri 'none'; form-action 'none'")
		_, _ = w.Write(data)
	}
}
