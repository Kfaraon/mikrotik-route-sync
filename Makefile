# ==============================================================================
# Переменные проекта
# ==============================================================================
APP_NAME := mikrotik-route-sync
CMD_PATH := ./cmd/app
BIN_DIR := bin

# Версионирование (внедряется через -ldflags)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

# Путь к пакету с версиями (должен совпадать с internal/version/version.go)
VERSION_PKG := github.com/Kfaraon/mikrotik-route-sync/internal/version

# Флаги линковки
LDFLAGS := -s -w \
	-X '$(VERSION_PKG).Version=$(VERSION)' \
	-X '$(VERSION_PKG).Commit=$(COMMIT)' \
	-X '$(VERSION_PKG).BuildDate=$(BUILD_DATE)'

# Флаги сборки (статический бинарник, без отладочной информации)
BUILD_FLAGS := -trimpath -ldflags="$(LDFLAGS)"

# Docker
DOCKER_IMAGE := $(APP_NAME)
DOCKER_TAG ?= $(VERSION)

# ==============================================================================
# Цели по умолчанию
# ==============================================================================
.DEFAULT_GOAL := help
.PHONY: help build release test lint fmt vet sec docker docker-push clean run

# ==============================================================================
# Справка
# ==============================================================================
help: ## Показать эту справку
	@echo "Доступные цели:"
	@awk 'BEGIN {FS = ":.*##"; printf "\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ==============================================================================
# Сборка
# ==============================================================================
build: ## Собрать бинарник для текущей платформы (с внедрением версии)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(APP_NAME) $(CMD_PATH)
	@echo "✅ Собран: $(BIN_DIR)/$(APP_NAME) (version=$(VERSION), commit=$(COMMIT))"

release: ## Собрать production-релиз (Linux amd64, статический бинарник)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BIN_DIR)/$(APP_NAME)-linux-amd64 $(CMD_PATH)
	@echo "✅ Релиз собран: $(BIN_DIR)/$(APP_NAME)-linux-amd64"

# ==============================================================================
# Тестирование и качество кода
# ==============================================================================
test: ## Запустить тесты с покрытием
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	@echo "📊 Покрытие:"
	@go tool cover -func=coverage.out | tail -n 1

test-html: ## Сгенерировать HTML-отчет о покрытии
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "📄 Отчет: coverage.html"

fmt: ## Отформатировать код (gofmt)
	gofmt -s -w cmd internal pkg
	@echo "✅ Код отформатирован"

vet: ## Запустить go vet (статический анализ)
	go vet ./...
	@echo "✅ go vet пройден"

lint: ## Запустить golangci-lint (требует установленный golangci-lint)
	@if ! command -v golangci-lint &> /dev/null; then \
		echo "❌ golangci-lint не установлен. Установите: https://golangci-lint.run/usage/install/"; \
		exit 1; \
	fi
	golangci-lint run ./...
	@echo "✅ golangci-lint пройден"

# ==============================================================================
# Безопасность
# ==============================================================================
sec: ## Запустить проверку безопасности (gosec + govulncheck)
	@echo "🔒 Запуск gosec..."
	@if ! command -v gosec &> /dev/null; then \
		echo "❌ gosec не установлен. Установите: go install github.com/securego/gosec/v2/cmd/gosec@latest"; \
		exit 1; \
	fi
	gosec -exclude-dir=vendor -quiet ./...
	
	@echo "🔒 Запуск govulncheck..."
	@if ! command -v govulncheck &> /dev/null; then \
		echo "❌ govulncheck не установлен. Установите: go install golang.org/x/vuln/cmd/govulncheck@latest"; \
		exit 1; \
	fi
	govulncheck ./...
	
	@echo "✅ Проверки безопасности пройдены"

# ==============================================================================
# Docker
# ==============================================================================
docker: ## Собрать Docker-образ
	docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) -t $(DOCKER_IMAGE):latest .
	@echo "✅ Образ собран: $(DOCKER_IMAGE):$(DOCKER_TAG)"

docker-push: ## Опубликовать Docker-образ (требует docker login)
	docker push $(DOCKER_IMAGE):$(DOCKER_TAG)
	docker push $(DOCKER_IMAGE):latest

docker-run: ## Запустить контейнер локально (с томами для данных и логов)
	docker run --rm -it \
		-p 8080:8080 \
		-v $(PWD)/data:/data \
		-v $(PWD)/logs:/var/log/mikrotik-route-sync \
		--name $(APP_NAME) \
		$(DOCKER_IMAGE):latest

# ==============================================================================
# Модули и зависимости
# ==============================================================================
deps: ## Загрузить и очистить зависимости
	go mod download
	go mod tidy
	go mod verify
	@echo "✅ Зависимости обновлены"

# ==============================================================================
# Запуск
# ==============================================================================
run: build ## Собрать и запустить локально (режим daemon)
	./$(BIN_DIR)/$(APP_NAME) daemon --config config.yaml

run-web: build ## Собрать и запустить только веб-интерфейс
	./$(BIN_DIR)/$(APP_NAME) web --config config.yaml

# ==============================================================================
# Очистка
# ==============================================================================
clean: ## Очистить артефакты сборки
	rm -rf $(BIN_DIR)
	rm -f coverage.out coverage.html
	@echo "🧹 Очищено"

# ==============================================================================
# Полный цикл CI
# ==============================================================================
ci: fmt vet lint test sec ## Запустить полный цикл проверки (как в CI/CD)
	@echo "🎉 Все проверки CI пройдены"
