package core

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// TestFailureReasonPreservesCausalErrorsAndIndependentValues проверяет совместимость errors.Is
// и отсутствие общей изменяемой причины между вызывающим кодом, оболочкой и её читателями.
func TestFailureReasonPreservesCausalErrorsAndIndependentValues(t *testing.T) {
	// reason содержит проверенные идентификаторы уже прочитанных объектов.
	reason := FailureReason{Code: FailureFallbackProfileNotPrepared, Field: FailureFieldProfile, Context: FailureContext{StreamID: "stream", GroupID: "group", ProfileID: "profile", AssetID: "asset", GenerationID: "generation"}}
	// original сохраняет независимый ожидаемый снимок причины до любых изменений копий.
	original := reason
	// cause сохраняет исходную диагностику и прежний доменный sentinel.
	cause := fmt.Errorf("synthetic-native-diagnostic: %w", ErrNotReady)
	// wrapped добавляет структурированное объяснение без потери исходного диагностического cause.
	wrapped := WithFailureReason(cause, reason)
	reason.Context.GroupID = "changed-input"
	if !errors.Is(wrapped, ErrNotReady) || wrapped.Error() != cause.Error() || errors.Unwrap(wrapped) != cause {
		t.Fatal("оболочка потеряла исходную ошибку", wrapped)
	}
	// extracted содержит первую полученную копию, которую потребитель вправе изменить.
	extracted := FailureReasonFrom(wrapped)
	if extracted == nil || *extracted != original {
		t.Fatal("входная причина не была скопирована", extracted)
	}
	extracted.Context.ProfileID = "changed-result"
	// got повторно читает оболочку после изменения ранее полученной копии.
	if got := FailureReasonFrom(wrapped); got == nil || *got != original {
		t.Fatal("читатель изменил доверенную причину", got)
	}
}

// TestFailureReasonJoinsNeverCombineDifferentExplanations проверяет неоднозначные ветви Join
// и разрешает одинаковую причину под обычной диагностической оболочкой.
func TestFailureReasonJoinsNeverCombineDifferentExplanations(t *testing.T) {
	// first и second представляют разные доказанные причины одного ErrNotReady.
	first := FailureReason{Code: FailureFallbackNotConfigured, Field: FailureFieldFallback}
	second := FailureReason{Code: FailureFallbackNotPrepared, Field: FailureFieldFallback}
	// scenario задаёт дерево ошибок и независимо ожидаемую единственную причину либо её отсутствие.
	for _, scenario := range []struct {
		// name обозначает форму дерева причин.
		name string
		// failure содержит исходные диагностические оболочки.
		failure error
		// want задаёт однозначное ожидаемое объяснение; nil запрещает выбирать случайную ветвь.
		want *FailureReason
	}{
		{"plain", ErrNotReady, nil},
		{"diagnostic text is not code", errors.New("fallback_not_configured"), nil},
		{"wrapped", fmt.Errorf("boundary: %w", WithFailureReason(ErrNotReady, first)), &first},
		{"same", errors.Join(WithFailureReason(ErrNotReady, first), WithFailureReason(ErrNotReady, first)), &first},
		{"different", errors.Join(WithFailureReason(ErrNotReady, first), WithFailureReason(ErrNotReady, second)), nil},
		{"reverse", errors.Join(WithFailureReason(ErrNotReady, second), WithFailureReason(ErrNotReady, first)), nil},
	} {
		// Подтест проверяет извлечение единственной причины для формы дерева scenario.
		t.Run(scenario.name, func(t *testing.T) {
			// got содержит итоговую причину целиком, а не набор полей разных ветвей.
			got := FailureReasonFrom(scenario.failure)
			if !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("получено %+v вместо %+v", got, scenario.want)
			}
		})
	}
}

// TestFailureReasonRejectsUnknownCodesAndFields запрещает расширять доверенный каталог через
// произвольный текст диагностики, имя поля либо непредставимый идентификатор.
func TestFailureReasonRejectsUnknownCodesAndFields(t *testing.T) {
	// reason содержит один из недопустимых вариантов закрытого публичного контракта.
	for _, reason := range []FailureReason{
		{},
		{Code: FailureCode("synthetic-secret")},
		{Code: FailureRequestIDConflict, Field: FailureField("synthetic-secret")},
		{Code: FailureRevisionConflict, Field: FailureFieldRequestID},
		{Code: FailureSessionStartFailed, Context: FailureContext{SessionID: "invalid\x00id"}},
	} {
		// err содержит результат проверки закрытого каталога и допустимости ссылок.
		if err := reason.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("недопустимая причина прошла проверку: %+v %v", reason, err)
		}
		// got выявляет ошибочно доверенную причину после отклонения её значений.
		if got := FailureReasonFrom(WithFailureReason(ErrNotReady, reason)); got != nil {
			t.Fatalf("недопустимая причина стала доверенной: %+v", got)
		}
	}
	// got выявляет создание доверенной причины при отсутствующем исходном отказе.
	if got := FailureReasonFrom(WithFailureReason(nil, FailureReason{Code: FailureSessionStartFailed})); got != nil {
		t.Fatal("причина создана без исходного отказа", got)
	}
}
