package codec

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestNativeAAC71ASCNormalization проверяет узкую границу исправления собственного native AAC:
// меняются только четыре бита известного ASC, обычный разбор config7 сохраняет wide-back.
func TestNativeAAC71ASCNormalization(t *testing.T) {
	// tc задаёт исходные байты, точный результат и признак допустимого представления.
	for _, tc := range []struct {
		// raw и expected содержат самостоятельные ASC в шестнадцатеричном виде; accepted задаёт результат.
		raw, expected string
		accepted      bool
	}{
		{"11b856e500", "11e056e500", true},
		{"123856e500", "126056e500", true},
		{"11e056e500", "11e056e500", true},
		{"11e0", "11e0", true},
		{"11b8", "", false},
		{"119056e500", "", false},
		{"11b856e501", "", false},
		{"11b856e50000", "", false},
		{"11b9", "", false},
	} {
		// raw содержит отдельный входной ASC, err сообщает о повреждении тестовой константы.
		raw, err := hex.DecodeString(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		// before сохраняет исходные байты для проверки отсутствия побочной записи.
		before := bytes.Clone(raw)
		// normalized содержит результат узкой коррекции без изменения исходного среза.
		normalized, err := NormalizeNativeAAC71ASC(raw)
		if (err == nil) != tc.accepted || !bytes.Equal(before, raw) || hex.EncodeToString(normalized) != tc.expected {
			t.Fatalf("ASC %s → %x, %v", tc.raw, normalized, err)
		}
	}
	// pce задаёт нормативную 7.1 с отдельными front, side, back и LFE элементами.
	pce := pceFixture([4][]uint32{{0, 0x10}, {0x11}, {0x12}, {0}}, nil)
	// normalized и err проверяют неизменность уже корректного явного PCE.
	normalized, err := NormalizeNativeAAC71ASC(pce)
	if err != nil || !bytes.Equal(normalized, pce) {
		t.Fatal("изменён нормативный PCE 7.1", err)
	}
	// forced моделирует неверный native forced-PCE без LFE, а не нормативный 7.1.
	forced := pceFixture([4][]uint32{{0x10, 0}, {1}, {0x11, 0x12}, nil}, nil)
	if _, err = NormalizeNativeAAC71ASC(forced); err == nil {
		t.Fatal("принят forced PCE без LFE")
	}
}

// pceFixture строит AAC-LC 48 kHz с независимо заданными типами и tags четырёх групп PCE.
// Каждая запись первых трёх групп хранит is_cpe в старшем бите и четырёхбитный tag.
func pceFixture(groups [4][]uint32, comment []byte) []byte {
	// b собирает ASC, полный PCE и отключённое расширение SBR по синтаксическим полям.
	b := ppsBits{}
	b.put(0x1180, 16)
	b.put(0, 4)
	b.put(1, 2)
	b.put(3, 4)
	// index выбирает группу, group содержит её записи канальных элементов.
	for index, group := range groups {
		// width задаёт четыре бита счётчика либо два бита для LFE.
		width := 4
		if index == 3 {
			width = 2
		}
		b.put(uint32(len(group)), width)
	}
	b.put(0, 7) // Нет assoc_data и valid_cc.
	b.put(0, 3) // Нет дополнительных сведений о сведении каналов.
	// index выбирает группу, group содержит type/tag записи в порядке PCE.
	for index, group := range groups {
		// element содержит is_cpe/tag либо один tag для LFE.
		for _, element := range group {
			if index == 3 {
				b.put(element, 4)
			} else {
				b.put(element, 5)
			}
		}
	}
	for b.count%8 != 0 {
		b.put(0, 1)
	}
	b.put(uint32(len(comment)), 8)
	// value содержит очередной байт самостоятельного комментария PCE.
	for _, value := range comment {
		b.put(uint32(value), 8)
	}
	b.put(0x2b7, 11)
	b.put(5, 5)
	b.put(0, 1)
	return b.raw
}

// TestAACPCESpeakerMapAndTail проверяет speaker карту, произвольные tags и полный хвост PCE.
func TestAACPCESpeakerMapAndTail(t *testing.T) {
	// tc задаёт канальные элементы PCE и независимо ожидаемую раскладку.
	for _, tc := range []struct {
		// groups задаёт front, side, back, LFE элементы.
		groups [4][]uint32
		// layout задаёт ожидаемую раскладку; пустая строка требует отказа.
		layout string
	}{
		{[4][]uint32{{0x1f}, nil, nil, {9}}, "2.1"},
		{[4][]uint32{{0x12, 8}, nil, nil, nil}, "3.0"},
		{[4][]uint32{{0x12, 8}, nil, {9}, nil}, "4.0"},
		{[4][]uint32{{0x12, 8}, nil, {0x13}, nil}, "5.0"},
		{[4][]uint32{{0x12, 8}, nil, {0x13}, {0}}, "5.1"},
		{[4][]uint32{{0x12, 8}, nil, {0x12}, nil}, ""}, // Повтор одного CPE tag в двух группах.
		{[4][]uint32{{0x12}, nil, {0x13}, nil}, ""},    // Quad не является 4.0.
		{[4][]uint32{{0x12, 8}, {9}, {0x13}, nil}, ""}, // Одиночный side не определяет LFE.
		{[4][]uint32{{8, 9}, nil, nil, nil}, ""},       // Два SCE не объявляют стереопару.
	} {
		// raw содержит полный ASC с непрозрачным комментарием, before сохраняет его исходные байты.
		raw := pceFixture(tc.groups, []byte("comment"))
		before := bytes.Clone(raw)
		// format содержит фактическую карту, err сообщает об отказе разбора.
		format, err := AudioFormatASC(raw)
		if (err == nil) != (tc.layout != "") || err == nil && format.ChannelLayout != tc.layout || !bytes.Equal(raw, before) {
			t.Fatalf("PCE %+v: %+v, %v", tc.groups, format, err)
		}
	}
	// raw содержит максимальный комментарий PCE; truncated перебирает обрывы обязательных полей.
	raw := pceFixture([4][]uint32{{0x10}, nil, nil, {0}}, bytes.Repeat([]byte{0xff}, 255))
	// err сообщает об отказе чтения максимально длинного допустимого PCE.
	if _, err := AudioFormatASC(raw); err != nil {
		t.Fatal(err)
	}
	for truncated := 0; truncated < len(raw)-3; truncated++ {
		// err содержит отказ для очередного обрыва обязательного тела PCE.
		if _, err := AudioFormatASC(raw[:truncated]); err == nil {
			t.Fatalf("принято усечение PCE на байте %d", truncated)
		}
	}
	// err содержит результат проверки недопустимого дополнительного байта.
	if _, err := AudioFormatASC(append(bytes.Clone(raw), 0)); err == nil {
		t.Fatal("приняты лишние данные после полного ASC")
	}
}

// TestAACEmptyMultichannelMap проверяет все канальные элементы тишины, их tags и обязательный ID_END.
func TestAACEmptyMultichannelMap(t *testing.T) {
	// raw содержит ASC 2.1 с CPE tag15 и LFE tag9, а b собирает независимую AAC AU тишины.
	raw := pceFixture([4][]uint32{{0x1f}, nil, nil, {9}}, nil)
	b := ppsBits{}
	b.put(1, 3)
	b.put(15, 4)
	b.put(0, 1) // common_window отсутствует.
	for range 2 {
		b.put(0, 8)  // global_gain.
		b.put(0, 11) // Длинное окно и max_sfb=0.
		b.put(0, 3)  // Нет pulse, TNS и gain control.
	}
	// missing сохраняет первую пару без обязательного LFE.
	missing := b
	missing.raw = bytes.Clone(b.raw)
	missing.put(7, 3)
	// err содержит результат проверки AU с отсутствующим LFE.
	if err := ValidateAACAU(raw, missing.raw); err == nil {
		t.Fatal("ASC 2.1 принят без LFE")
	}
	b.put(3, 3)
	b.put(9, 4)
	b.put(0, 8)
	b.put(0, 11)
	b.put(0, 3)
	b.put(7, 3)
	// err сообщает об отказе проверки полной карты CPE/LFE.
	if err := ValidateAACAU(raw, b.raw); err != nil {
		t.Fatal(err)
	}
	// damaged меняет tag LFE с9 на8 при неизменном ASC.
	damaged := bytes.Clone(b.raw)
	damaged[7] ^= 1 << 5
	// err содержит результат проверки AU с недопустимым tag LFE.
	if err := ValidateAACAU(raw, damaged); err == nil {
		t.Fatal("принят LFE с tag вне PCE")
	}
}
