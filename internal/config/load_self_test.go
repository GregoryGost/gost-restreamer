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

func TestDefaultYAMLRoundTrip(t *testing.T) {
	want := Default()
	data, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("YAML изменил настройки по умолчанию")
	}
}
func TestYAMLScalarTypesAndMergedMappings(t *testing.T) {
	for _, tc := range []struct {
		content string
		valid   bool
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
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}
func TestConfigurationRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Load(path); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO принят как конфигурация")
		}
	case <-time.After(time.Second):
		t.Fatal("загрузка ждёт писателя FIFO")
	}
}

func TestAddressSyntaxValidationWithoutLookup(t *testing.T) {
	for _, address := range []string{":8080", "localhost:8080", "[::1]:8080", "[fe80::1%en0]:8080", "host.example.:8080"} {
		c := Default()
		c.HTTP.Address = address
		if err := c.Validate(); err != nil {
			t.Fatalf("%s: %v", address, err)
		}
	}
	for _, address := range []string{"bad!host:8080", "-host:8080", "a..b:8080", "host:0", "host:http", "[bad::host]:8080"} {
		c := Default()
		c.HTTP.Address = address
		if c.Validate() == nil {
			t.Fatalf("ошибочный адрес принят: %s", address)
		}
	}
}
func TestSmallerProducerBudgetsFitSharedOwners(t *testing.T) {
	c := Default()
	c.API.UploadBytes /= 2
	c.GPU.MaxDevices /= 2
	c.GPU.MaxUUIDBytes /= 2
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
