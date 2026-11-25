package app

import (
	"context"
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/chat"
	"github.com/GregoryGost/gost-restreamer/internal/chat/twitch"
	"github.com/GregoryGost/gost-restreamer/internal/chat/vkvideolive"
	"github.com/GregoryGost/gost-restreamer/internal/chat/youtube"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// acquireChat регистрирует адаптеры с общими настройками и передаёт Manager во владение приложению.
// Фоновая сверка продолжает работу после отмены контекста запуска. Закрытие Manager запрашивает
// её остановку одновременно с HTTP и удерживает Store до завершения всех обращений к нему.
func (a *Application) acquireChat() error {
	// limits задаёт единые бюджеты адаптеров и Manager для всего срока работы приложения.
	limits := a.config.Chat
	// youtubeAdapter ограничивает discovery и чтение чата YouTube; err сообщает об отказе сборки.
	youtubeAdapter, err := youtube.New(limits)
	if err != nil {
		return fmt.Errorf("чат YouTube: %w", err)
	}
	// twitchAdapter обеспечивает Twitch Device Code и чтение EventSub в пределах тех же бюджетов.
	twitchAdapter, err := twitch.New(limits)
	if err != nil {
		return fmt.Errorf("чат Twitch: %w", err)
	}
	// vkAdapter обслуживает DevAPI и чат VK Видео Live с ограниченными сетевыми операциями.
	vkAdapter, err := vkvideolive.New(limits)
	if err != nil {
		return fmt.Errorf("чат VK Видео Live: %w", err)
	}
	a.chat, err = chat.New(context.Background(), a.core, a.store, limits, []core.ChatAdapterPort{youtubeAdapter, twitchAdapter, vkAdapter})
	if a.chat != nil {
		a.owners.chat = &stopOwner{name: "chat", close: func(ctx context.Context) stopResult {
			// Запрашивает остановку чатов и подтверждает выход всех операций отдельным сигналом Done.
			// closeErr хранит результат ожидания, в том числе истечение срока до фактического выхода.
			closeErr := a.chat.Close(ctx)
			return stopResult{closeErr, isClosed(a.chat.Done())}
		}}
	}
	return err
}
