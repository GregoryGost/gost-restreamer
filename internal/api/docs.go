package api

import (
	"net/http"

	"github.com/oaswrap/spec-ui"
	"github.com/oaswrap/spec-ui/scalaremb"
)

// openAPIJSONPath задаёт общий адрес опубликованной спецификации и источника операций Scalar.
const openAPIJSONPath = Prefix + "/openapi.json"

// registerDocs подключает страницу Scalar и встроенные файлы scalaremb. Страница читает спецификацию
// и отправляет запросы только на сервер, с которого загружена.
func (a *API) registerDocs() {
	// docsPath задаёт адрес страницы, а assetsPath — префикс URL встроенных файлов.
	const docsPath = Prefix + "/docs"
	const assetsPath = docsPath + "/_assets"
	// docs использует встроенные файлы выбранной версии Scalar без загрузки с CDN.
	docs := specui.NewHandler(specui.WithAssetsPath(assetsPath), scalaremb.WithUI())

	// pageHTML инициализирует Scalar с локальной спецификацией. Настройки выключают шрифты CDN,
	// внешний клиент, инструменты публикации и агент Scalar, которому можно передать спецификацию.
	const pageHTML = `<!doctype html>
<html lang="ru">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Управляющий API — Scalar</title>
  <link rel="stylesheet" href="` + assetsPath + `/style.min.css">
  <link rel="icon" type="image/png" href="` + assetsPath + `/favicon.png">
</head>
<body>
  <div id="app"></div>
  <script src="` + assetsPath + `/browser/standalone.min.js"></script>
  <script>
    Scalar.createApiReference('#app', {
      url: new URL("` + openAPIJSONPath + `", window.location.origin).href,
      withDefaultFonts: false,
      hideTestRequestButton: false,
      hideClientButton: true,
      showDeveloperTools: 'never',
      agent: { disabled: true }
    })
  </script>
</body>
</html>`
	// page выдаёт неизменяемую HTML-страницу с локальными адресами ресурсов.
	page := immutable("text/html; charset=utf-8", []byte(pageHTML))
	// Обработчик GET выдаёт в w страницу для запроса r и ограничивает подключения браузера её адресом.
	a.router.Get(docsPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "connect-src 'self'")
		page(w, r)
	})

	// assets отдаёт файлы из встроенной файловой системы Scalar.
	assets := docs.Assets()
	// serveAsset выдаёт в w запрошенный встроенный файл. Отказы файлового обработчика
	// оформляются как Problem Details с сохранением статуса HTTP.
	serveAsset := func(w http.ResponseWriter, r *http.Request) {
		// response передаёт успешный ответ клиенту и задерживает отправку отказа файлового обработчика.
		response := &docsAssetResponse{ResponseWriter: w}
		assets.ServeHTTP(response, r)
		if response.errorStatus != 0 {
			writeProblem(w, response.errorStatus)
		}
	}
	// name задаёт имя файла, указанного в HTML. Неизвестные пути обрабатывает общий маршрут 404.
	for _, name := range []string{"/style.min.css", "/favicon.png", "/browser/standalone.min.js"} {
		a.router.Get(assetsPath+name, serveAsset)
	}
}

// docsAssetResponse передаёт успешную выдачу файла клиенту и удерживает статус отказа,
// чтобы обработчик маршрута мог записать Problem Details вместо ответа файлового сервера.
type docsAssetResponse struct {
	// ResponseWriter передаёт заголовки и байты ответа клиенту.
	http.ResponseWriter
	// errorStatus хранит статус отказа файлового сервера. Ноль означает обычную выдачу.
	errorStatus int
}

// WriteHeader передаёт status клиенту при обычной выдаче и задерживает отправку статуса ошибки.
func (w *docsAssetResponse) WriteHeader(status int) {
	if status >= http.StatusBadRequest {
		w.errorStatus = status
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write отправляет data клиенту при обычной выдаче. Тело отказа файлового сервера
// поглощается целиком, чтобы его мог заменить Problem Details.
func (w *docsAssetResponse) Write(data []byte) (int, error) {
	if w.errorStatus != 0 {
		return len(data), nil
	}
	return w.ResponseWriter.Write(data)
}
