package fallback

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestSourceVP9RecordUsesActualBoundedBox проверяет извлечение реальной записи конфигурации без подстановки желаемых значений,
// а также отказ усечённым, повторным и превышающим бюджет описаниям.
func TestSourceVP9RecordUsesActualBoundedBox(t *testing.T) {
	// record содержит фактическую конфигурацию profile2/10-bit/PQ синтетического нативного VP9.
	record := []byte{2, 10, 0xa0, 9, 16, 9, 0, 0}
	// config содержит vpcC FullBox с version1/flags0 перед исходной записью record.
	config := sourceTestBox("vpcC", append([]byte{1, 0, 0, 0}, record...))
	// entry содержит фиксированную видеозапись и одну исходную конфигурацию кодека.
	entry := sourceTestBox("vp09", append(make([]byte, 78), config...))
	// stsd содержит одно описание дорожки после version/flags и entry_count.
	stsd := append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, entry...)
	// moov содержит только фиксированный фактический путь к описанию дорожки.
	moov := sourceTestBox("stsd", stsd)
	// kind последовательно добавляет внешний контейнер метаданных каждого следующего уровня.
	for _, kind := range []string{"stbl", "minf", "mdia", "trak"} {
		moov = sourceTestBox(kind, moov)
	}
	// actual/err содержат точную запись либо отказ структурного/числового контракта.
	actual, err := sourceVP9Record(moov, 8)
	if err != nil || !bytes.Equal(actual, record) {
		t.Fatal(actual, err)
	}
	if _, err = sourceVP9Record(moov, 7); err == nil {
		t.Fatal("превышение бюджета record принято")
	}
	if _, err = sourceVP9Record(moov[:len(moov)-1], 8); err == nil {
		t.Fatal("усечённый moov принят")
	}
	if _, err = sourceVP9Record(append(append([]byte(nil), moov...), moov...), 8); err == nil {
		t.Fatal("два независимых описания VP9 приняты")
	}
	// malformed обозначает очередной невозможный размер блока.
	for _, malformed := range [][]byte{{0}, {0, 0, 0, 4, 'v', 'p', 'c', 'C'}, {0, 0, 0, 1, 'v', 'p', 'c', 'C'}, {0, 0, 0, 9, 'v', 'p', 'c', 'C'}} {
		if _, err = sourceBoxBodies(malformed, "vpcC"); err == nil {
			t.Fatal(malformed)
		}
	}
}

// sourceTestBox создаёт конечный синтетический MP4 box kind с body для локальных тестов разбора.
func sourceTestBox(kind string, body []byte) []byte {
	// box содержит полный размер, четырёхбайтовый тип и независимую копию содержимого.
	box := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(box, uint32(len(box)))
	copy(box[4:8], kind)
	copy(box[8:], body)
	return box
}
