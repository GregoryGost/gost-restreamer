.DEFAULT_GOAL := help

GO := go
GO_VERSION := $(shell sed -n 's/^go //p' go.mod)
GOLANGCI_LINT_VERSION := 2.15.0
GOLANGCI_LINT := $(CURDIR)/bin/golangci-lint

export GOTOOLCHAIN := local
export GOWORK := off
export CGO_ENABLED := 1
export GOFLAGS ?= -mod=readonly

.PHONY: help toolchain-check tools require-linter require-packages fmt fmt-check \
	tidy tidy-check mod-verify vet test coverage test-race lint-config lint build binary \
	bootstrap-check check

help:
	@printf '%s\n' \
		'make tools            — установить закреплённый golangci-lint в ./bin.' \
		'make bootstrap-check  — проверить модуль, форматирование и конфигурацию линтера.' \
		'make check            — выполнить все проверки Go-пакетов и сборку.' \
		'make coverage         — выполнить тесты и проверить суммарное покрытие не ниже 85%.' \
		'make fmt              — отформатировать Go-файлы.' \
		'make tidy             — обновить go.mod и go.sum по импортам.' \
		'make binary           — собрать сервис в ./bin/gost-restreamer.'

toolchain-check:
	@$(GO) version
	@test "$$($(GO) env GOVERSION)" = "go$(GO_VERSION)" || { \
		printf '%s\n' 'Нужен Go $(GO_VERSION), указанный в go.mod.' >&2; exit 1; \
	}

tools: toolchain-check
	mkdir -p bin
	GOBIN="$(CURDIR)/bin" $(GO) install -trimpath \
		-ldflags='-X main.version=$(GOLANGCI_LINT_VERSION)' \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION)

require-linter:
	@test -x "$(GOLANGCI_LINT)" || { \
		printf '%s\n' 'Сначала выполните make tools.' >&2; exit 1; \
	}
	@version="$$("$(GOLANGCI_LINT)" version --short)" || exit $$?; \
		test "$$version" = "$(GOLANGCI_LINT_VERSION)" || { \
		printf '%s\n' 'Версия golangci-lint не совпадает с Makefile; выполните make tools.' >&2; exit 1; \
	}

require-packages: toolchain-check
	@packages="$$($(GO) list ./...)" || exit $$?; \
		test -n "$$packages" || { \
			printf '%s\n' 'Go-пакетов пока нет; доступна только make bootstrap-check.' >&2; exit 1; \
		}

fmt: toolchain-check
	gofmt -w .

fmt-check: toolchain-check
	@files="$$(gofmt -l .)" || exit $$?; \
		test -z "$$files" || { \
			printf '%s\n' "$$files" 'Выполните make fmt.' >&2; exit 1; \
		}

tidy: toolchain-check
	$(GO) mod tidy

tidy-check: toolchain-check
	$(GO) mod tidy -diff

mod-verify: toolchain-check
	$(GO) mod verify

vet: require-packages
	$(GO) vet ./...

test: require-packages
	$(GO) test ./...

coverage: require-packages
	@coverage_dir="$$(mktemp -d "$${TMPDIR:-/tmp}/gost-restreamer-coverage.XXXXXXXX")" || exit $$?; \
		trap 'coverage_status=$$?; trap - EXIT HUP INT TERM; rm -rf "$$coverage_dir"; exit "$$coverage_status"' EXIT; \
		trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM; \
		profile="$$coverage_dir/coverage.out"; \
		printf '%s\n' 'Покрытие всех пакетов ./...; исключений нет; порог 85%.' "Профиль текущего запуска: $$profile"; \
		GOST_MEDIA_FFMPEG= GOST_FMP4_FFMPEG= GOST_MEDIA_VIDEOTOOLBOX= $(GO) test -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$$profile" ./... || exit $$?; \
		test -s "$$profile" || { printf '%s\n' 'NOT VERIFIED: профиль покрытия отсутствует или пуст.' >&2; exit 1; }; \
		$(GO) tool cover -func="$$profile" || exit $$?; \
		$(GO) build -o "$$coverage_dir/coveragecheck" ./tools/coveragecheck || exit $$?; \
		"$$coverage_dir/coveragecheck" -profile "$$profile" -min 85

test-race: require-packages
	$(GO) test -race ./...

lint-config: require-linter
	"$(GOLANGCI_LINT)" config verify

lint: require-packages lint-config
	"$(GOLANGCI_LINT)" run ./...

build: require-packages
	$(GO) build -o /dev/null ./...

binary: require-packages
	mkdir -p bin
	$(GO) build -trimpath -o bin/gost-restreamer ./cmd/gost-restreamer

bootstrap-check: toolchain-check fmt-check tidy-check mod-verify lint-config

check: bootstrap-check vet coverage test-race lint build
