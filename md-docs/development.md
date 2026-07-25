# Разработка

## Требования

* Go версии, указанной в `go.mod` (проверяется CI через `go-version-file`);
* для полного прогона тестов — бинарник `xray` в `$PATH` (часть тестов `internal/vpn`
  запускает ядро);
* Docker — только для сборки и проверки образа.

## Сборка

```bash
make build          # ./build/xraytool под текущую платформу
make build-linux    # статический linux/amd64 (CGO_ENABLED=0)
make install        # установка в /usr/local/bin/xraytool
make tidy           # go mod tidy
make clean
```

Флаги линковки `-s -w` убирают отладочную информацию и уменьшают размер бинарника.

## Проверки перед коммитом

```bash
gofmt -l .          # должен ничего не вывести
go vet ./...
go build ./...
go test ./...
```

## Структура репозитория

```
main.go                — точка входа, вызывает cmd.Execute()
cmd/                   — команды CLI и композиционный корень
internal/              — вся реализация (см. таблицу пакетов в architecture.md)
tests/                 — интеграционные и e2e-тесты
md-docs/               — документация
Dockerfile             — многоступенчатая сборка образа
Dockerfile.ci          — образ для CI (бинарник собирается на раннере)
docker-compose.yml     — прод-развертывание с Watchtower
Makefile               — цели сборки
```

## Принципы, которых стоит держаться

1. **Порты и адаптеры.** Бизнес-логика зависит только от интерфейсов `internal/domain`.
   Импорт типов Xray-core допустим исключительно в `internal/vpn`.
2. **Транзакционность.** Любая операция, меняющая подписку и журнал синхронизации,
   выполняется через `Registry.WithTx`.
3. **Атомарная запись файлов.** Конфиги и ключи пишутся через `internal/safeio`
   (временный файл + `rename`), чтобы падение процесса не оставило битый `config.json`.
4. **Никаких «сырых» IP и секретов в логах.** IP хешируются, заголовки с ключами
   маскируются.
5. **Ошибки не глотаем**, а оборачиваем: `fmt.Errorf("контекст: %w", err)`.

## Тесты

```bash
go test ./...                          # всё
go test ./internal/subscription/...    # один пакет
go test -run TestProcessSQL -v ./internal/subscription
go test -race ./internal/antifraud/...
```

Что нужно знать:

* тесты `internal/vpn` требуют исполняемого `xray` в `PATH` и без него падают;
* e2e-набор в `tests/` поднимает настоящий HTTP-сервер и тоже зависит от окружения;
* для портов есть готовые моки в `internal/mocks` — новые зависимости добавляйте
  интерфейсом, а не конкретным типом, иначе юнит-тест написать не получится.

## Добавление нового VPN-ядра

1. Реализуйте `domain.Engine` в новом пакете (по образцу `internal/vpn`).
2. Добавьте ветку в `switch cfg.Engine.Type` в `cmd/server.go` и `cmd/root.go`.
3. Всё остальное — API, воркеры, синхронизация — менять не потребуется.

## Добавление REST-эндпоинта

1. Обработчик — в подходящий `internal/server/handlers_*.go`.
2. Маршрут — в `internal/server/router.go` (публичный или под `authMiddleware`).
3. Тест — рядом, по образцу существующих `*_test.go`.
4. Описание запроса/ответа — в [api_documentation.md](api_documentation.md).

## CI

Workflow `.github/workflows/docker-publish.yml` при пуше в `main`:

1. собирает бинарник (`CGO_ENABLED=0 GOOS=linux`);
2. собирает образ по `Dockerfile.ci`;
3. публикует в GHCR с тегами `latest` и `sha-<короткий_хеш>`.

На проде Watchtower подхватывает новый `latest` в течение пяти минут.
