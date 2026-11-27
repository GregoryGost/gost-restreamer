package core

import (
	"fmt"
	"regexp"
)

// chatAppearanceColor разрешает только шестизначную запись RGB для передачи цвета в CSS.
var chatAppearanceColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// ChatBackgroundAppearance хранит параметры одного фона, в том числе цвет и прозрачность выключенного фона.
type ChatBackgroundAppearance struct {
	// Enabled показывает, рисует ли страница этот фон.
	Enabled bool `json:"enabled"`
	// Color содержит цвет RGB в шестнадцатеричной записи.
	Color string `json:"color"`
	// Transparency задаёт процент прозрачности фона. Значение 100 скрывает фон полностью.
	Transparency int `json:"transparency"`
}

// ChatTextOutlineAppearance хранит общую обводку текста сообщений и имён авторов.
type ChatTextOutlineAppearance struct {
	// Enabled показывает, рисует ли страница обводку.
	Enabled bool `json:"enabled"`
	// Color содержит цвет обводки в шестнадцатеричной записи RGB.
	Color string `json:"color"`
	// WidthPx задаёт толщину обводки в пикселях CSS.
	WidthPx int `json:"width_px"`
	// Transparency задаёт процент прозрачности обводки независимо от заливки букв.
	Transparency int `json:"transparency"`
}

// ChatMessageTextAppearance хранит параметры заливки и размера текста сообщений.
type ChatMessageTextAppearance struct {
	// Color содержит цвет заливки в шестнадцатеричной записи RGB.
	Color string `json:"color"`
	// SizePx задаёт размер шрифта сообщения в пикселях CSS.
	SizePx int `json:"size_px"`
	// Transparency задаёт процент прозрачности заливки текста независимо от обводки.
	Transparency int `json:"transparency"`
}

// ChatAppearance хранит оформление Dashboard и Overlay для одного потока.
type ChatAppearance struct {
	// DashboardBackground описывает фон области чата в Dashboard.
	DashboardBackground ChatBackgroundAppearance `json:"dashboard_background"`
	// OverlayBackground описывает фон Overlay.
	OverlayBackground ChatBackgroundAppearance `json:"overlay_background"`
	// OverlayTransparency задаёт общую прозрачность Overlay в процентах.
	OverlayTransparency int `json:"overlay_transparency"`
	// MessageBackground описывает фон отдельных сообщений.
	MessageBackground ChatBackgroundAppearance `json:"message_background"`
	// TextOutline описывает обводку текста сообщений и имён авторов.
	TextOutline ChatTextOutlineAppearance `json:"text_outline"`
	// MessageText описывает цвет, прозрачность заливки и размер текста сообщений.
	MessageText ChatMessageTextAppearance `json:"message_text"`
}

// DefaultChatAppearance возвращает оформление для потока без сохранённых настроек чата.
func DefaultChatAppearance() ChatAppearance {
	return ChatAppearance{
		DashboardBackground: ChatBackgroundAppearance{Enabled: true, Color: "#101823"},
		OverlayBackground:   ChatBackgroundAppearance{Color: "#0d131d"},
		MessageBackground:   ChatBackgroundAppearance{Enabled: true, Color: "#0c121b", Transparency: 23},
		TextOutline:         ChatTextOutlineAppearance{Color: "#000000", WidthPx: 1},
		MessageText:         ChatMessageTextAppearance{Color: "#eff4fb", SizePx: 16},
	}
}

// EffectiveChatAppearance возвращает копию сохранённого оформления либо значения по умолчанию для чтения.
func (s Stream) EffectiveChatAppearance() ChatAppearance {
	if s.ChatAppearance != nil {
		return *s.ChatAppearance
	}
	return DefaultChatAppearance()
}

// Validate проверяет цвета RGB, проценты прозрачности и размеры в пикселях CSS.
// Цвет и прозрачность проверяются и у выключенных элементов, чтобы их можно было включить позже.
func (a ChatAppearance) Validate() error {
	// layer содержит цвет и прозрачность одного элемента вместе с его именем для диагностики.
	for _, layer := range []struct {
		// name содержит ключ элемента в настройках.
		name string
		// color содержит цвет элемента.
		color string
		// transparency содержит процент прозрачности элемента.
		transparency int
	}{
		{"dashboard_background", a.DashboardBackground.Color, a.DashboardBackground.Transparency},
		{"overlay_background", a.OverlayBackground.Color, a.OverlayBackground.Transparency},
		{"message_background", a.MessageBackground.Color, a.MessageBackground.Transparency},
		{"text_outline", a.TextOutline.Color, a.TextOutline.Transparency},
		{"message_text", a.MessageText.Color, a.MessageText.Transparency},
	} {
		if !chatAppearanceColor.MatchString(layer.color) {
			return fmt.Errorf("%w: ChatAppearance.%s.color должен иметь формат #RRGGBB", ErrInvalid, layer.name)
		}
		if layer.transparency < 0 || layer.transparency > 100 {
			return fmt.Errorf("%w: ChatAppearance.%s.transparency вне диапазона 0–100", ErrInvalid, layer.name)
		}
	}
	if a.OverlayTransparency < 0 || a.OverlayTransparency > 100 {
		return fmt.Errorf("%w: ChatAppearance.overlay_transparency вне диапазона 0–100", ErrInvalid)
	}
	if a.TextOutline.WidthPx < 0 || a.TextOutline.WidthPx > 8 {
		return fmt.Errorf("%w: ChatAppearance.text_outline.width_px вне диапазона 0–8", ErrInvalid)
	}
	if a.MessageText.SizePx < 8 || a.MessageText.SizePx > 72 {
		return fmt.Errorf("%w: ChatAppearance.message_text.size_px вне диапазона 8–72", ErrInvalid)
	}
	return nil
}
