package chat

import "github.com/GregoryGost/gost-restreamer/internal/core"

// testChatSample задаёт вымышленную реплику и атрибуты её отображения в предэфирной ленте.
type testChatSample struct {
	// provider выбирает значок одной из поддерживаемых площадок.
	provider core.ChatProvider
	// author содержит имя вымышленного зрителя и необязательный цвет имени.
	author core.ChatAuthor
	// text передаёт основную реплику, включая перенос строки, если он есть у автора.
	text string
	// emoji добавляет отдельный фрагмент реакции и входит в полный текст сообщения.
	emoji string
	// badge добавляет подпись роли или метку подписки рядом с автором.
	badge string
}

// testChatSamples содержит по четыре реплики каждой площадки. Hub перемешивает весь набор,
// поэтому каждая площадка появляется за один проход даже при отсутствии подключённых аккаунтов.
var testChatSamples = [...]testChatSample{
	{provider: core.ChatYouTube, author: core.ChatAuthor{DisplayName: "Лис в эфире"}, text: "Привет всем!", emoji: "👋"},
	{provider: core.ChatYouTube, author: core.ChatAuthor{DisplayName: "Кадр за кадром", Color: "#47c8ff"}, text: "Подскажите, во сколько начнётся основная часть? Хочу успеть вернуться с прогулки и не пропустить разбор ваших вопросов.", badge: "Постоянный зритель"},
	{provider: core.ChatYouTube, author: core.ChatAuthor{DisplayName: "Mira_21"}, text: "С телефона всё читается отлично", emoji: "👌"},
	{provider: core.ChatYouTube, author: core.ChatAuthor{DisplayName: "Соня ☕"}, text: "Успела за чаем!\nА запись потом будет доступна?"},
	{provider: core.ChatTwitch, author: core.ChatAuthor{DisplayName: "pixel_kot", Color: "#bd91ff"}, text: "Вот это тайминг", emoji: "😂", badge: "Подписчик"},
	{provider: core.ChatTwitch, author: core.ChatAuthor{DisplayName: "NightMod", Color: "#68dcaa"}, text: "Вопросы кидайте в чат, вернёмся к ним в конце.", badge: "Модератор"},
	{provider: core.ChatTwitch, author: core.ChatAuthor{DisplayName: "Rin"}, text: "gg", emoji: "🔥"},
	{provider: core.ChatTwitch, author: core.ChatAuthor{DisplayName: "moonwalk_42"}, text: "@pixel_kot я тоже подумал, что сейчас всё развалится. На повторе видно, что последний прыжок был прямо по краю."},
	{provider: core.ChatVKVideoLive, author: core.ChatAuthor{DisplayName: "Аня из Казани"}, text: "Звук есть, картинка тоже", emoji: "👍"},
	{provider: core.ChatVKVideoLive, author: core.ChatAuthor{DisplayName: "Вечерний зритель", Color: "#ffb475"}, text: "Первый раз здесь. Антон, спасибо, что объясняешь с примерами. На прошлом эфире я потерялся в самом начале, а сегодня всё понятно с первой минуты.", badge: "Подписчик"},
	{provider: core.ChatVKVideoLive, author: core.ChatAuthor{DisplayName: "Марсель"}, text: "О, знакомая музыка!", emoji: "🎵"},
	{provider: core.ChatVKVideoLive, author: core.ChatAuthor{DisplayName: "Алексей"}, text: "У меня звук чуть тише музыки.\nУ остальных так же?", emoji: "🎧"},
}

// incoming строит содержимое синтетической реплики. Отдельные фрагменты реакции и метки
// проходят по тому же пути отображения, что и данные от платформенных адаптеров.
func (s testChatSample) incoming() core.ChatIncomingMessage {
	// message сохраняет происхождение test и дополняется только атрибутами выбранной реплики.
	message := core.ChatIncomingMessage{Provider: core.ChatTest, Author: s.author, Text: s.text, Kind: core.ChatMessageText}
	if s.emoji != "" {
		message.Text += " " + s.emoji
		message.Parts = []core.ChatPart{{Kind: core.ChatPartText, Text: s.text + " "}, {Kind: core.ChatPartEmote, Text: s.emoji}}
	}
	if s.badge != "" {
		message.Badges = []core.ChatBadge{{Name: s.badge}}
	}
	return message
}
