.DEFAULT_GOAL := help

GO := go
GO_VERSION := $(shell sed -n 's/^go //p' go.mod)
GOLANGCI_LINT_VERSION := 2.15.0
GOLANGCI_LINT := $(CURDIR)/bin/golangci-lint
VERSION ?=
VERSION_LDFLAGS = $(if $(strip $(VERSION)),-X github.com/GregoryGost/gost-restreamer/internal/buildinfo.version=$(VERSION))
BIN_DIR ?= bin
DIST_DIR ?= dist
GORELEASER ?= goreleaser
GORELEASER_VERSION := 2.19.0
NODE ?= node

export GOTOOLCHAIN := local
export GOWORK := off
export CGO_ENABLED := 1
export GOFLAGS ?= -mod=readonly

.PHONY: help toolchain-check tools require-linter require-packages fmt fmt-check \
	tidy tidy-check mod-verify vet test coverage test-race lint-config lint build binary \
	bootstrap-check check process-helper package package-debian release-check

help:
	@printf '%s\n' \
		'make tools            — установить закреплённый golangci-lint в ./bin.' \
		'make bootstrap-check  — проверить модуль, форматирование и конфигурацию линтера.' \
		'make check            — выполнить все проверки Go-пакетов и сборку.' \
		'make coverage         — выполнить тесты и проверить суммарное покрытие не ниже 85%.' \
		'make fmt              — отформатировать Go-файлы.' \
		'make tidy             — обновить go.mod и go.sum по импортам.' \
		'make binary           — собрать сервис в ./bin/gost-restreamer.'
	@printf '%s\n' 'make process-helper   — собрать владельца native-процессов в ./bin/gost-process-helper.'
	@printf '%s\n' 'make package VERSION=… — собрать оба бинарника, архив поставки и проверенную SHA256.'
	@printf '%s\n' 'make package-debian VERSION=… — GoReleaser: .deb, архив и SHA256 для Debian 13/amd64.'
	@printf '%s\n' 'make release-check     — проверить конфигурации GoReleaser и безопасные повторы выпуска.'

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
		helper_dir="$$coverage_dir/helpers"; \
		helper_profile="$$coverage_dir/helpers.out"; \
		mkdir "$$helper_dir" || exit $$?; \
		printf '%s\n' 'Покрытие всех пакетов ./...; исключений нет; порог 85%.' "Профиль текущего запуска: $$profile"; \
		GOST_MEDIA_FFMPEG= GOST_FMP4_FFMPEG= GOST_MEDIA_VIDEOTOOLBOX= GOST_TEST_PROCESS_HELPER_COVERDIR="$$helper_dir" $(GO) test -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$$profile" ./... || exit $$?; \
		test -s "$$profile" || { printf '%s\n' 'NOT VERIFIED: профиль покрытия отсутствует или пуст.' >&2; exit 1; }; \
		test "$$(sed -n '1p' "$$profile")" = 'mode: atomic' && test -n "$$(sed -n '2p' "$$profile")" || { printf '%s\n' 'NOT VERIFIED: основной профиль должен содержать блоки в режиме atomic.' >&2; exit 1; }; \
		$(GO) tool covdata textfmt -hw -i="$$helper_dir" -o="$$helper_profile" || exit $$?; \
		test -s "$$helper_profile" && test "$$(sed -n '1p' "$$helper_profile")" = 'mode: atomic' && test -n "$$(sed -n '2p' "$$helper_profile")" || { printf '%s\n' 'NOT VERIFIED: профиль helper должен содержать реальные блоки в режиме atomic.' >&2; exit 1; }; \
		sed '1d' "$$helper_profile" >> "$$profile" || exit $$?; \
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
	mkdir -p "$(BIN_DIR)"
	$(GO) build -trimpath -ldflags="$(VERSION_LDFLAGS)" -o "$(BIN_DIR)/gost-restreamer" ./cmd/gost-restreamer

process-helper: require-packages
	mkdir -p "$(BIN_DIR)"
	$(GO) build -trimpath -ldflags="$(VERSION_LDFLAGS)" -o "$(BIN_DIR)/gost-process-helper" ./cmd/gost-process-helper

package: binary process-helper
	@test -n "$(VERSION)" || { printf '%s\n' 'Для архива требуется явная VERSION.' >&2; exit 1; }
	@set -eu; \
		archive="gost-restreamer-$(VERSION)-$$($(GO) env GOOS)-$$($(GO) env GOARCH).tar.gz"; \
		mkdir -p "$(DIST_DIR)"; \
		tar -czf "$(DIST_DIR)/$$archive" -C "$(abspath $(BIN_DIR))" gost-restreamer gost-process-helper \
			-C "$(CURDIR)" configs/example.yaml deploy/systemd/gost-restreamer.service; \
		if command -v sha256sum >/dev/null 2>&1; then checksum=sha256sum; else checksum='shasum -a 256'; fi; \
		(cd "$(DIST_DIR)" && $$checksum "$$archive" > "$$archive.sha256" && $$checksum -c "$$archive.sha256"); \
		verify_dir="$$(mktemp -d "$${TMPDIR:-/tmp}/gost-restreamer-package.XXXXXXXX")"; \
		trap 'package_status=$$?; trap - EXIT HUP INT TERM; rm -rf "$$verify_dir"; exit "$$package_status"' EXIT; \
		trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM; \
		tar -xzf "$(DIST_DIR)/$$archive" -C "$$verify_dir"; \
		for binary in gost-restreamer gost-process-helper; do \
			test -x "$$verify_dir/$$binary"; cmp "$(BIN_DIR)/$$binary" "$$verify_dir/$$binary"; \
		done; \
		for asset in configs/example.yaml deploy/systemd/gost-restreamer.service; do \
			cmp "$$asset" "$$verify_dir/$$asset"; \
		done

package-debian: toolchain-check
	@test "$$($(GO) env GOHOSTOS)/$$($(GO) env GOHOSTARCH)" = linux/amd64 || { \
		printf '%s\n' 'Для Debian-пакета нужен нативный компилятор Linux/amd64.' >&2; exit 1; \
	}
	@set -eu; test -r /etc/os-release; . /etc/os-release; \
		test "$$ID:$$VERSION_ID" = debian:13 || { \
			printf '%s\n' 'Для Debian-пакета нужна Debian 13 (trixie).' >&2; exit 1; \
		}
	@test "$$($(GO) env GOOS)/$$($(GO) env GOARCH)/$$($(GO) env CGO_ENABLED)" = linux/amd64/1 || { \
		printf '%s\n' 'Пакет собирается нативно на Debian 13/linux/amd64 с CGO=1.' >&2; exit 1; \
	}
	@test "$$($(GORELEASER) --version | sed -n 's/^GitVersion: *//p')" = "$(GORELEASER_VERSION)"
	@set -eu; \
		printf '%s\n' '$(VERSION)' | LC_ALL=C grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-dev\.[1-9][0-9]*\+[0-9a-f]{7,40})?$$'; \
		test -z "$$(git status --porcelain)" || { printf '%s\n' 'Для поставки нужно чистое дерево Git.' >&2; exit 1; }; \
		test "$$(git rev-parse 'refs/tags/$(VERSION)^{commit}')" = "$$(git rev-parse HEAD)"; \
		debian_version="$$(printf '%s' '$(VERSION)' | sed 's/^v//; s/-dev\./~dev./')"; \
		set --; case '$(VERSION)' in *-dev.*) set -- --snapshot ;; esac; \
		VERSION='$(VERSION)' DEB_VERSION="$$debian_version" GORELEASER_CURRENT_TAG='$(VERSION)' \
			$(GORELEASER) release --config .goreleaser.yaml --clean --skip=publish "$$@"

release-check:
	$(GORELEASER) check --config .goreleaser.yaml
	$(GORELEASER) check --config .goreleaser-publish.yaml
	$(NODE) --test .github/scripts/release.test.cjs

bootstrap-check: toolchain-check fmt-check tidy-check mod-verify lint-config

check: bootstrap-check vet coverage test-race lint build
