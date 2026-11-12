package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// Проверяет, что сериализация значений по умолчанию в YAML и повторная строгая загрузка сохраняют всю
// конфигурацию.
func TestDefaultYAMLRoundTrip(t *testing.T) {
	// want содержит встроенную конфигурацию, которая должна пережить сериализацию и строгую загрузку
	// без изменений.
	want := Default()
	// data содержит сериализованный YAML настроек; err содержит отказ его формирования.
	data, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	// path указывает временный файл для полного цикла YAML-сериализации и загрузки.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	// got содержит конфигурацию после чтения сериализованных defaults.
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("YAML изменил настройки по умолчанию")
	}
}

// Проверяет строгие типы скаляров и корректные YAML-слияния, отклоняя неизвестные поля, null и
// циклические алиасы.
func TestYAMLScalarTypesAndMergedMappings(t *testing.T) {
	// tc задаёт документ со строгими скалярами или слияниями и ожидаемую допустимость.
	for _, tc := range []struct {
		// content содержит YAML со скалярами, слиянием или алиасом, проверяемый в подслучае.
		content string
		// valid определяет, должен ли документ успешно пройти строгую загрузку.
		valid bool
	}{
		{"http:\n  address: 8080\n", false},
		{"storage:\n  max_open_conns: 4.0\n", false},
		{"executables:\n  ffmpeg: false\n", false},
		{"logging:\n  level: INFO\n", true},
		{"logging:\n  level: false\n", false},
		{"http: &settings\n  idle_timeout: 7s\n", true},
		{"http:\n  <<: &settings {idle_timeout: 7s}\n", true},
		{"http:\n  <<: &settings {unknown: 7s}\n", false},
		{"http:\n  <<: &settings {idle_timeout: null}\n", false},
		{"http: &settings\n  <<: *settings\n", false},
	} {
		t.Run(strings.ReplaceAll(tc.content, "\n", "/"), func(t *testing.T) {
			// Проверяет строгую типизацию YAML и обработку объединённых отображений для выбранного документа.

			// path указывает изолированный файл текущего сценария типов и YAML-слияний.
			path := filepath.Join(t.TempDir(), "config.yml")
			// err содержит отказ записи YAML с проверяемыми типами или алиасами.
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			// err содержит результат строгой загрузки текущего документа для сравнения с
			// valid.
			_, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

// Проверяет отклонение FIFO как конфигурационного файла без ожидания писателя.
func TestConfigurationRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	// path указывает FIFO, который загрузчик должен отклонить без ожидания писателя.
	path := filepath.Join(t.TempDir(), "config.yaml")
	// err содержит отказ создания FIFO для проверки неблокирующего открытия.
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	// done передаёт результат загрузки FIFO для ограниченного ожидания в тесте.
	done := make(chan error, 1)
	go func() {
		// Пробует загрузить FIFO отдельно, позволяя тесту ограничить ожидание и обнаружить блокировку до
		// появления писателя.
		// err содержит отказ загрузки FIFO в отдельной горутине.
		_, err := Load(path)
		done <- err
	}()
	select {
	// err содержит полученный результат загрузки FIFO, который должен быть ошибкой.
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO принят как конфигурация")
		}
	case <-time.After(time.Second):
		t.Fatal("загрузка ждёт писателя FIFO")
	}
}

// Проверяет синтаксис адресов IPv4, IPv6, DNS и портов без DNS-запросов или привязки сокета.
func TestAddressSyntaxValidationWithoutLookup(t *testing.T) {
	// address задаёт очередной синтаксически допустимый адрес без сетевого разрешения или привязки
	// сокета.
	for _, address := range []string{":8080", "localhost:8080", "[::1]:8080", "[fe80::1%en0]:8080", "host.example.:8080"} {
		// c содержит отдельную копию defaults с текущим допустимым HTTP-адресом.
		c := Default()
		c.HTTP.Address = address
		// err содержит отказ проверки синтаксически допустимого адреса.
		if err := c.Validate(); err != nil {
			t.Fatalf("%s: %v", address, err)
		}
	}
	// address задаёт очередное неверное имя узла или порт, которые должны отклоняться локальной
	// проверкой.
	for _, address := range []string{"bad!host:8080", "-host:8080", "a..b:8080", "host:0", "host:http", "[bad::host]:8080"} {
		// c содержит настройки с текущим неверным HTTP-адресом.
		c := Default()
		c.HTTP.Address = address
		if c.Validate() == nil {
			t.Fatalf("ошибочный адрес принят: %s", address)
		}
	}
}

// Проверяет допустимость меньших лимитов загрузки файлов и GPU, когда они помещаются в общие бюджеты
// владельцев.
func TestSmallerProducerBudgetsFitSharedOwners(t *testing.T) {
	// c содержит настройки с уменьшенными лимитами загрузки и GPU внутри общих бюджетов владельцев.
	c := Default()
	c.API.UploadBytes /= 2
	c.GPU.MaxDevices /= 2
	c.GPU.MaxUUIDBytes /= 2
	// err содержит отказ проверки согласованных уменьшенных лимитов производителей.
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
