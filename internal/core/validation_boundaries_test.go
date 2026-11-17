package core_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestSessionTransitionsPreserveOneRunLifecycle проверяет переходы одного запуска:
// завершённая Session сохраняет результат и не возвращается к работе. t сообщает о нарушениях.
func TestSessionTransitionsPreserveOneRunLifecycle(t *testing.T) {
	// states задаёт порядок состояний в строках и столбцах матрицы, включая неизвестные значения.
	states := []core.SessionState{
		core.SessionStarting, core.SessionRunning, core.SessionStopping,
		core.SessionStopped, core.SessionFailed, core.SessionInterrupted,
		"", "UNKNOWN",
	}
	// allowed фиксирует допустимость переходов: строки соответствуют исходному состоянию,
	// столбцы — следующему. Неизвестное состояние недопустимо даже при переходе в себя.
	allowed := [][]bool{
		{true, true, true, false, true, true, false, false},
		{false, true, true, false, true, true, false, false},
		{false, false, true, true, true, true, false, false},
		{false, false, false, true, false, false, false, false},
		{false, false, false, false, true, false, false, false},
		{false, false, false, false, false, true, false, false},
		{false, false, false, false, false, false, false, false},
		{false, false, false, false, false, false, false, false},
	}
	// row выбирает строку матрицы для исходного состояния from.
	for row, from := range states {
		// column выбирает столбец матрицы для следующего состояния to.
		for column, to := range states {
			// Обработчик подслучая через t сравнивает отдельный переход с матрицей жизненного цикла.
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				// got содержит фактическое решение о допустимости перехода.
				if got := core.CanTransitionSession(from, to); got != allowed[row][column] {
					t.Fatalf("переход Session %q → %q = %t, ожидается %t", from, to, got, allowed[row][column])
				}
			})
		}
	}
}

// TestOutputTransitionsRequireConnectingBeforeRestart проверяет повторное подключение выхода:
// после остановки или отказа публикация требует CONNECTING. t сообщает о нарушениях переходов.
func TestOutputTransitionsRequireConnectingBeforeRestart(t *testing.T) {
	// states задаёт порядок состояний соединения в обоих измерениях матрицы.
	states := []core.OutputState{
		core.OutputConnecting, core.OutputPublishing, core.OutputRetrying,
		core.OutputFailed, core.OutputStopped, "", "UNKNOWN",
	}
	// allowed задаёт допустимые переходы между состояниями соединения, включая повторный запуск
	// и отказ для пустого или неизвестного состояния.
	allowed := [][]bool{
		{true, true, true, true, true, false, false},
		{false, true, true, true, true, false, false},
		{true, true, true, true, true, false, false},
		{true, false, false, true, true, false, false},
		{true, false, false, false, true, false, false},
		{false, false, false, false, false, false, false},
		{false, false, false, false, false, false, false},
	}
	// row выбирает строку матрицы для исходного состояния from.
	for row, from := range states {
		// column выбирает столбец матрицы для следующего состояния to.
		for column, to := range states {
			// Обработчик подслучая через t проверяет отдельный переход состояния соединения.
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				// got содержит решение о переходе выхода в следующее состояние.
				if got := core.CanTransitionOutput(from, to); got != allowed[row][column] {
					t.Fatalf("переход Output %q → %q = %t, ожидается %t", from, to, got, allowed[row][column])
				}
			})
		}
	}
}

// TestVideoEncodingValidatesIndependentPositiveBounds проверяет базовые числовые ограничения
// отдельно от возможностей конкретного кодировщика. t сообщает о неверном допуске или отказе.
func TestVideoEncodingValidatesIndependentPositiveBounds(t *testing.T) {
	// cases проверяет каждое обязательное поле независимо, соотношение битрейтов и границы типов.
	cases := []struct {
		// name обозначает проверяемое граничное условие.
		name string
		// value содержит самостоятельный набор базовых параметров видеокодирования.
		value core.VideoEncoding
		// valid указывает, должен ли базовый валидатор принять этот набор параметров.
		valid bool
	}{
		{"minimum", core.VideoEncoding{TargetBitrateBPS: 1, MaxBitrateBPS: 1, BufferBits: 1, GOPFrames: 1}, true},
		{"target_below_maximum", core.VideoEncoding{TargetBitrateBPS: 1, MaxBitrateBPS: 2, BufferBits: 1, GOPFrames: 1}, true},
		{"unsigned_limits", core.VideoEncoding{TargetBitrateBPS: math.MaxUint64, MaxBitrateBPS: math.MaxUint64, BufferBits: math.MaxUint64, GOPFrames: math.MaxUint32}, true},
		{"zero_target", core.VideoEncoding{MaxBitrateBPS: 1, BufferBits: 1, GOPFrames: 1}, false},
		{"zero_maximum", core.VideoEncoding{TargetBitrateBPS: 1, BufferBits: 1, GOPFrames: 1}, false},
		{"target_above_maximum", core.VideoEncoding{TargetBitrateBPS: 2, MaxBitrateBPS: 1, BufferBits: 1, GOPFrames: 1}, false},
		{"zero_buffer", core.VideoEncoding{TargetBitrateBPS: 1, MaxBitrateBPS: 1, GOPFrames: 1}, false},
		{"zero_gop", core.VideoEncoding{TargetBitrateBPS: 1, MaxBitrateBPS: 1, BufferBits: 1}, false},
	}
	// tc содержит вход и ожидаемую допустимость текущего набора параметров.
	for _, tc := range cases {
		// Обработчик подслучая через t проверяет категорию ошибки и неизменность входа.
		t.Run(tc.name, func(t *testing.T) {
			// before сохраняет исходные параметры для сравнения после проверки.
			before := tc.value
			// err содержит результат базовой проверки без выбора кодировщика.
			err := tc.value.Validate()
			if tc.valid && err != nil {
				t.Fatalf("допустимые базовые параметры отклонены: %v", err)
			}
			if !tc.valid && !errors.Is(err, core.ErrInvalidProfile) {
				t.Fatalf("недопустимые базовые параметры приняты: %v", err)
			}
			if tc.value != before {
				t.Fatal("проверка изменила параметры кодирования")
			}
		})
	}
}

// TestAudioEncodingRequiresPositiveBitrate проверяет положительность базового аудиобитрейта.
// Правило нулевого битрейта FLAC применяется полным профилем отдельно. t сообщает о нарушениях.
func TestAudioEncodingRequiresPositiveBitrate(t *testing.T) {
	// bitrate перебирает ноль, минимальное положительное значение и верхнюю границу типа.
	for _, bitrate := range []uint64{0, 1, math.MaxUint64} {
		// value содержит базовые параметры аудиокодирования без привязки к кодеку.
		value := core.AudioEncoding{BitrateBPS: bitrate}
		// err содержит результат проверки положительности битрейта.
		err := value.Validate()
		if bitrate == 0 && !errors.Is(err, core.ErrInvalidProfile) {
			t.Fatalf("нулевой базовый аудиобитрейт принят: %v", err)
		}
		if bitrate != 0 && err != nil {
			t.Fatalf("положительный базовый аудиобитрейт %d отклонён: %v", bitrate, err)
		}
		if value.BitrateBPS != bitrate {
			t.Fatal("проверка изменила аудиобитрейт")
		}
	}
}

// TestControlEventRejectsIncompleteIdentityAndTime проверяет обязательные поля служебного события
// и представимость его времени; связь с выходом необязательна. t сообщает о неверной валидации.
func TestControlEventRejectsIncompleteIdentityAndTime(t *testing.T) {
	// base задаёт корректное событие управления с постоянным временем UTC.
	base := core.Event{ID: "event", SessionID: "session", Kind: "command_completed", Message: "Команда завершена", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	// output выбирает событие всей Session либо отдельного выхода.
	for _, output := range []core.ID{"", "output"} {
		// value содержит копию корректного события с выбранной областью действия.
		value := base
		value.OutputID = output
		// err содержит результат проверки допустимого события.
		if err := value.Validate(); err != nil {
			t.Fatalf("допустимое событие для выхода %q отклонено: %v", output, err)
		}
	}
	// cases задаёт отдельные нарушения обязательных полей и представимости времени.
	cases := []struct {
		// name обозначает нарушенное ограничение события.
		name string
		// edit получает копию события через v и вносит одно нарушение для изолированной проверки.
		edit func(*core.Event)
	}{
		{"event_id", func(v *core.Event) { v.ID = "" }},
		{"session_id", func(v *core.Event) { v.SessionID = "" }},
		{"output_id", func(v *core.Event) { v.OutputID = "\x00" }},
		{"kind", func(v *core.Event) { v.Kind = " \t" }},
		{"message", func(v *core.Event) { v.Message = "" }},
		{"missing_time", func(v *core.Event) { v.CreatedAt = time.Time{} }},
		{"time_overflow", func(v *core.Event) { v.CreatedAt = time.Unix(math.MaxInt64, 0) }},
	}
	// tc выбирает изменение одного поля корректного события.
	for _, tc := range cases {
		// Обработчик подслучая через t проверяет категорию отказа для выбранного нарушения.
		t.Run(tc.name, func(t *testing.T) {
			// value содержит отдельную копию события, чтобы нарушения не накапливались.
			value := base
			tc.edit(&value)
			// err содержит отказ валидации события с одним нарушенным ограничением.
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("неполное служебное событие принято: %v", err)
			}
		})
	}
}

// TestDestinationRejectsInvalidReferencesAndText проверяет ссылки и допустимость текста назначения,
// сохраняя разрешённый пустой ключ публикации. t сообщает о неверном допуске или отказе.
func TestDestinationRejectsInvalidReferencesAndText(t *testing.T) {
	// base задаёт корректное назначение с синтетическим адресом без сетевого обращения.
	base := core.Destination{StreamID: "stream", GroupID: "group", Name: "Назначение", Platform: "custom", Endpoint: "rtmps://example.invalid/live"}
	// err содержит результат проверки допустимого назначения с пустым ключом.
	if err := base.Validate(); err != nil {
		t.Fatalf("корректное назначение с пустым ключом отклонено: %v", err)
	}
	// cases задаёт отдельные нарушения ссылок, обязательного текста и кодировки ключа.
	cases := []struct {
		// name обозначает нарушенное ограничение назначения.
		name string
		// edit получает отдельную копию назначения через v и меняет одно проверяемое поле.
		edit func(*core.Destination)
	}{
		{"stream_id", func(v *core.Destination) { v.StreamID = "" }},
		{"group_id", func(v *core.Destination) { v.GroupID = "" }},
		{"name", func(v *core.Destination) { v.Name = "\u2003" }},
		{"platform", func(v *core.Destination) { v.Platform = "" }},
		{"key_nul", func(v *core.Destination) { v.StreamKey = "key\x00suffix" }},
		{"key_utf8", func(v *core.Destination) { v.StreamKey = string([]byte{0xff}) }},
	}
	// tc выбирает одно нарушение в копии корректного назначения.
	for _, tc := range cases {
		// Обработчик подслучая через t проверяет категорию отказа для выбранного поля.
		t.Run(tc.name, func(t *testing.T) {
			// value содержит отдельное назначение, чтобы нарушения не влияли на соседние случаи.
			value := base
			tc.edit(&value)
			// err содержит отказ валидации назначения с изменённым полем.
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("назначение с недопустимым текстом или ссылкой принято: %v", err)
			}
		})
	}
}

// TestArtifactManifestRejectsMissingGenerationAndUnverifiedFormat проверяет обязательное поколение,
// структурную корректность и совместимость измеренного формата. t сообщает о неверном допуске.
func TestArtifactManifestRejectsMissingGenerationAndUnverifiedFormat(t *testing.T) {
	// snapshot предоставляет согласованный снимок выхода с проверенным файлом заглушки.
	_, snapshot := contractSnapshots(t)
	// base содержит корректный manifest до независимого изменения его полей.
	base := snapshot.Artifact
	// err содержит результат проверки исходного manifest.
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	// missingGeneration моделирует файл без идентичности поколения-владельца.
	missingGeneration := base
	missingGeneration.GenerationID = ""
	// err содержит отказ при отсутствии поколения.
	if err := missingGeneration.Validate(); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("manifest без поколения принят: %v", err)
	}
	// invalidFormat моделирует измеренный формат с отсутствующей обязательной шириной кадра.
	invalidFormat := base
	invalidFormat.VerifiedFormat.Video.Width = 0
	// err содержит структурную ошибку формата, сохраняемую при проверке manifest.
	if err := invalidFormat.Validate(); !errors.Is(err, core.ErrInvalidFormat) {
		t.Fatalf("manifest без проверенного размера видео принят: %v", err)
	}
	// incompatibleFormat моделирует структурно корректный формат, не совпадающий с профилем файла.
	incompatibleFormat := base
	incompatibleFormat.VerifiedFormat.Video.Width += 2
	// err содержит отказ совместимости измеренного формата с полным профилем.
	if err := incompatibleFormat.Validate(); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("manifest с несовместимым проверенным форматом принят: %v", err)
	}
}
