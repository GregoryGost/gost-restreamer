package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет тип MetadataError, категорию ErrInvalidMetadata и доступность исходной причины через
// цепочку ошибок.
func requireMetadataError(t *testing.T, err error) {
	t.Helper()
	// metadata принимает типизированную ошибку для проверки общей категории и исходной причины.
	var metadata *core.MetadataError
	if !errors.Is(err, core.ErrInvalidMetadata) || !errors.As(err, &metadata) || metadata.Cause == nil || metadata.Error() == "" {
		t.Fatalf("потеря категории или причины ошибки метаданных: %v", err)
	}
	if !errors.Is(err, metadata.Cause) {
		t.Fatalf("причина ошибки не доступна через errors.Is: %v", err)
	}
}

// Проверяет точное сохранение исторического снимка выхода и независимость восстановленных канонических
// байтов.
func TestOutputSnapshotMetadataPreservesHistoricalConfiguration(t *testing.T) {
	// expected задаёт исходное значение, которое должно сохраниться после кодирования и чтения.
	_, expected := contractSnapshots(t)
	expected.Destination.Revision = math.MaxUint64
	expected.Artifact.SizeBytes = math.MaxUint64
	expected.Profile.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 120, Denominator: 2}
	expected.Artifact.Profile = expected.Profile.Profile
	// key содержит ключ полного профиля исторического manifest. canonical содержит канонические
	// байты полного профиля исторического manifest. err сообщает об ошибке вычисления канонической
	// идентичности профиля.
	key, canonical, err := core.ProfileIdentity(expected.Artifact.Profile)
	if err != nil {
		t.Fatal(err)
	}
	expected.Artifact.ProfileKey, expected.Artifact.CanonicalProfile = key, canonical
	// originalCanonical сохраняет отдельную эталонную копию канонических байтов для проверки, что
	// изменение восстановленного снимка не меняет исходный буфер.
	originalCanonical := bytes.Clone(canonical)
	// data содержит проверяемое JSON-представление доменного значения.
	data, err := core.EncodeOutputSnapshot(expected)
	if err != nil {
		t.Fatal(err)
	}
	// actual принимает восстановленное значение для точного сравнения с исходным.
	actual, err := core.DecodeOutputSnapshot(data)
	if err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("исторический снимок изменён после хранения: %+v, %v", actual, err)
	}
	actual.Artifact.CanonicalProfile[0] ^= 1
	if !bytes.Equal(expected.Artifact.CanonicalProfile, originalCanonical) {
		t.Fatal("восстановленный снимок изменил исходные байты manifest")
	}
}

// Проверяет сохранение необязательных исходных дорожек и точных строковых байтов без подмены форматом
// выходного профиля.
func TestSourceMetadataPreservesObservedOriginalInsteadOfOutputProfile(t *testing.T) {
	// raw содержит строку с пробелами, некорректным UTF-8 и нулевым байтом для проверки точного
	// хранения.
	raw := string([]byte{' ', 0xff, 0, 'x', ' '})
	// video задаёт исходную видеодорожку, намеренно отличную от выходного профиля.
	video := core.VideoFormat{Codec: "source-video-codec", Width: 73, Height: 41, FrameRate: core.FrameRate{Numerator: 120, Denominator: 2}, CodecProfile: raw, Level: raw, Tier: raw, PixelFormat: raw, BitDepth: 12, Color: core.ColorInfo{Range: raw, Primaries: raw, Transfer: raw, Matrix: raw}}
	// audio задаёт исходную аудиодорожку, намеренно отличную от выходного профиля.
	audio := core.AudioFormat{Codec: "source-audio-codec", CodecProfile: raw, SampleRateHz: 44100, Channels: 1, ChannelLayout: raw}
	// expected задаёт исходное значение, которое должно сохраниться после кодирования и чтения.
	for _, expected := range []core.SourceFormat{
		{}, {ImageFormat: "png"}, {Video: &video}, {Audio: &audio}, {Video: &video, Audio: &audio},
	} {
		// data содержит проверяемое JSON-представление доменного значения. err сообщает об
		// ошибке кодирования проверяемого JSON-представления.
		data, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		// actual принимает восстановленное значение для точного сравнения с исходным.
		var actual core.SourceFormat
		// err сообщает об ошибке разбора JSON проверяемого представления.
		if err := json.Unmarshal(data, &actual); err != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("описание исходника изменено: %+v, %v", actual, err)
		}
	}
}

// Проверяет типизированный отказ форматов и исходных метаданных при повреждении, неизвестных полях,
// версиях и недопустимых значениях.
func TestTypedMediaMetadataRejectsCorruption(t *testing.T) {
	// format задаёт корректный медиаформат для последующего повреждения его представления.
	format := validProfile().Format
	// formatData содержит корректное исходное представление медиаформата. err сообщает об ошибке
	// кодирования медиаформата.
	formatData, err := core.EncodeMediaFormat(format)
	if err != nil {
		t.Fatal(err)
	}
	// source задаёт исходный формат с обеими дорожками и изображением.
	source := core.SourceFormat{Video: &format.Video, Audio: &format.Audio, ImageFormat: "png"}
	// sourceData содержит корректное исходное представление SourceFormat.
	sourceData, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	// tc содержит выбранный декодер и его корректную исходную нагрузку.
	for _, tc := range []struct {
		// name задаёт имя проверяемого вида метаданных.
		name string
		// data содержит корректные исходные байты выбранного медиаформата.
		data []byte
		// run запускает выбранный декодер для общей проверки типизированного отказа.
		run func([]byte) error
	}{
		{"format", formatData, func(data []byte) error {
			// Пробует прочитать MediaFormat в копию исходного корректного формата для общей проверки повреждённых
			// метаданных.

			// actual принимает восстановленное значение для точного сравнения с исходным.
			actual := format
			return actual.UnmarshalJSON(data)
		}},
		{"source", sourceData, func(data []byte) error {
			// Пробует прочитать SourceFormat с собственными копиями аудио- и видеоописаний для общей проверки
			// повреждённых метаданных.

			// receiverFormat создаёт собственные копии дорожек получателя для проверки
			// чтения SourceFormat.
			receiverFormat := format
			// actual принимает восстановленное значение для точного сравнения с исходным.
			actual := core.SourceFormat{Video: &receiverFormat.Video, Audio: &receiverFormat.Audio, ImageFormat: "png"}
			return actual.UnmarshalJSON(data)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет типизированный отказ выбранного медиадекодера на повреждённом JSON, неизвестных версиях и
			// полях и лишнем содержимом после документа.

			// input содержит очередное повреждённое, неполное или неподдерживаемое
			// представление.
			for _, input := range [][]byte{
				nil, []byte("null"), []byte("{"), []byte(`{"Version":1,"Unknown":true}`),
				bytes.Replace(tc.data, []byte(`"Version":1`), []byte(`"Version":2`), 1),
				bytes.Replace(tc.data, []byte(`"Video":{`), []byte(`"Video":{"Unknown":true,`), 1),
				bytes.Replace(tc.data, []byte(`"Codec":"av1"`), []byte(`"Codec":""`), 1),
				append(bytes.Clone(tc.data), []byte(" true")...),
				append(bytes.Clone(tc.data), []byte(" {")...),
			} {
				requireMetadataError(t, tc.run(input))
			}
		})
	}
	// invalidFormat содержит копию медиаформата с недопустимой нулевой шириной.
	invalidFormat := format
	invalidFormat.Video.Width = 0
	_, err = core.EncodeMediaFormat(invalidFormat)
	requireMetadataError(t, err)
	// invalidSource содержит копию SourceFormat с недопустимым текстом формата изображения.
	invalidSource := source
	invalidSource.ImageFormat = "\x00"
	_, err = json.Marshal(invalidSource)
	requireMetadataError(t, err)
}

// Проверяет отказ кодирования и чтения повреждённых либо несогласованных снимков Session и Output.
func TestSnapshotMetadataRejectsCorruptAndInconsistentSnapshots(t *testing.T) {
	// session задаёт корректный начальный снимок запуска. output задаёт корректный самостоятельный
	// снимок выхода.
	session, output := contractSnapshots(t)
	// sessionData содержит корректные байты снимка запуска до внесения повреждений. err сообщает об
	// ошибке кодирования снимка запуска.
	sessionData, err := core.EncodeSessionSnapshot(session)
	if err != nil {
		t.Fatal(err)
	}
	// outputData содержит корректные байты снимка выхода до внесения повреждений.
	outputData, err := core.EncodeOutputSnapshot(output)
	if err != nil {
		t.Fatal(err)
	}
	// tc содержит выбранный декодер и его корректную исходную нагрузку.
	for _, tc := range []struct {
		// name задаёт имя проверяемого вида метаданных.
		name string
		// data содержит корректные исходные байты выбранного снимка.
		data []byte
		// run запускает выбранный декодер для общей проверки типизированного отказа.
		run func([]byte) error
	}{
		{"session", sessionData, func(data []byte) error {
			// Возвращает ошибку DecodeSessionSnapshot для общей проверки повреждения и несогласованности снимков.
			// err сообщает об ошибке чтения и проверки снимка запуска.
			_, err := core.DecodeSessionSnapshot(data)
			return err
		}},
		{"output", outputData, func(data []byte) error {
			// Возвращает ошибку DecodeOutputSnapshot для общей проверки повреждения и несогласованности снимков.
			// err сообщает об ошибке чтения и проверки снимка выхода.
			_, err := core.DecodeOutputSnapshot(data)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет типизированный отказ выбранного декодера снимка при повреждённом JSON, чужом StreamID,
			// неизвестной версии или дополнительном документе.

			// input содержит очередное повреждённое, неполное или неподдерживаемое
			// представление.
			for _, input := range [][]byte{
				[]byte("{"), []byte("null"), []byte(`{"Version":1,"Snapshot":{},"Unknown":1}`),
				bytes.Replace(tc.data, []byte(`"Version":1`), []byte(`"Version":2`), 1),
				bytes.Replace(tc.data, []byte(`"StreamID":"stream"`), []byte(`"StreamID":"foreign"`), 1),
				append(bytes.Clone(tc.data), []byte(" {}")...),
				append(bytes.Clone(tc.data), []byte(" !")...),
			} {
				requireMetadataError(t, tc.run(input))
			}
		})
	}
	session.Stream.Name = ""
	_, err = core.EncodeSessionSnapshot(session)
	requireMetadataError(t, err)
	output.Artifact.ContentHash = ""
	_, err = core.EncodeOutputSnapshot(output)
	requireMetadataError(t, err)
}

// Проверяет точное сохранение uint64 и обычных типизированных значений, отклоняя неподдерживаемые
// оболочки, поля и нагрузки.
func TestDomainMetadataPreservesNumbersAndRejectsUnsupportedEnvelope(t *testing.T) {
	// expected задаёт исходное значение, которое должно сохраниться после кодирования и чтения.
	expected := contractReceipt()
	expected.Lifecycle, expected.Attempt = math.MaxUint64, math.MaxUint64
	expected.Status = core.CommandClaimed
	// data содержит проверяемое JSON-представление доменного значения. err сообщает об ошибке
	// кодирования доменных метаданных.
	data, err := core.EncodeMetadata(expected)
	if err != nil {
		t.Fatal(err)
	}
	// actual принимает восстановленное значение для точного сравнения с исходным.
	var actual core.CommandReceipt
	// err сообщает об ошибке чтения и проверки доменных метаданных.
	if err := core.DecodeMetadata(data, &actual); err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("квитанция изменилась после хранения: %+v, %v", actual, err)
	}
	// input содержит очередное повреждённое, неполное или неподдерживаемое представление.
	for _, input := range [][]byte{
		[]byte("{"), []byte(`{"Version":999,"Payload":{}}`), []byte(`{"Version":1}`),
		[]byte(`{"Version":1,"Payload":null}`), []byte(`{"Version":1,"Payload":{},"Unknown":1}`),
		[]byte(`{"Version":1,"Payload":{"Unknown":1}}`), []byte(`{"Version":1,"Payload":{}}`),
		append(bytes.Clone(data), []byte(" {}")...),
		bytes.Replace(data, []byte(`"Lifecycle":18446744073709551615`), []byte(`"Lifecycle":18446744073709551616`), 1),
	} {
		// receiver принимает результат чтения повреждённой квитанции.
		var receiver core.CommandReceipt
		requireMetadataError(t, core.DecodeMetadata(input, &receiver))
	}
	_, err = core.EncodeMetadata(make(chan int))
	requireMetadataError(t, err)
	// plain задаёт обычное типизированное значение без Validate для проверки общей оболочки.
	plain := struct { /* Name содержит синтетический текст для проверки сохранения обычной структуры. */
		Name string
	}{Name: "Синтетические данные"}
	data, err = core.EncodeMetadata(plain)
	if err != nil {
		t.Fatal(err)
	}
	// decoded принимает обычную структуру после чтения версионированной оболочки.
	var decoded struct { /* Name содержит синтетический текст для проверки сохранения обычной структуры. */
		Name string
	}
	// err сообщает об ошибке чтения и проверки доменных метаданных.
	if err := core.DecodeMetadata(data, &decoded); err != nil || decoded != plain {
		t.Fatalf("обычное типизированное значение изменено: %+v, %v", decoded, err)
	}
}

// Проверяет диагностику вида и версии метаданных и доступность одновременно общей категории и исходной
// причины ошибки.
func TestMetadataErrorsExposeKindVersionAndCause(t *testing.T) {
	// cause задаёт обёрнутую исходную доменную причину ошибки метаданных.
	cause := fmt.Errorf("причина: %w", core.ErrInvalid)
	// err содержит типизированную ошибку метаданных с заданными видом, версией и исходной причиной.
	err := &core.MetadataError{Kind: "format", Version: 99, Cause: cause}
	if !errors.Is(err, core.ErrInvalid) || !errors.Is(err, core.ErrInvalidMetadata) || errors.Is(err, core.ErrNotFound) {
		t.Fatalf("неверная цепочка ошибок: %v", err)
	}
	// value содержит очередной обязательный фрагмент диагностики вида, версии или исходной причины.
	for _, value := range []string{"format", "99", cause.Error()} {
		if !strings.Contains(err.Error(), value) {
			t.Fatalf("диагностика не содержит %q: %v", value, err)
		}
	}
}
