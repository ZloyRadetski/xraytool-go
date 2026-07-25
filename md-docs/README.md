# Документация xraytool-go

Полный набор технической документации проекта. Начните с [обзора архитектуры](architecture.md),
если знакомитесь с проектом впервые, или сразу с [развертывания](deployment.md), если нужно
поднять кластер.

## Оглавление

| Документ | О чём |
| --- | --- |
| [architecture.md](architecture.md) | Архитектура: слои, порты и адаптеры, компоненты, потоки данных, роли Master/Slave |
| [deployment.md](deployment.md) | Установка и развертывание: Docker, бинарник, systemd, настройка Master и Slave-нод |
| [configuration.md](configuration.md) | Полный справочник по `config.yaml`: все секции, значения по умолчанию, валидация |
| [cli_reference.md](cli_reference.md) | Все команды CLI и их флаги с примерами |
| [api_documentation.md](api_documentation.md) | Справочник REST API с примерами запросов/ответов |
| [cluster_sync.md](cluster_sync.md) | Протокол синхронизации Master → Slave (ping / delta / full-sync), ротация Reality-ключей |
| [subscriptions.md](subscriptions.md) | Конвейер выдачи подписок: User-Agent, HWID, лимиты устройств, шаблоны и плейсхолдеры |
| [antifraud.md](antifraud.md) | Модуль антифрода: парсинг access-лога, подсчёт IP, софт-баны, агрегация со Slave-нод |
| [database_schema.md](database_schema.md) | Схема БД: таблицы, поля, связи, миграции |
| [webhooks.md](webhooks.md) | Исходящие вебхуки: формат событий, подпись HMAC, ретраи |
| [development.md](development.md) | Сборка, тесты, структура репозитория, соглашения, CI |
| [troubleshooting.md](troubleshooting.md) | Диагностика типовых проблем |
| [docker_management.md](docker_management.md) | Повседневное управление контейнерами |
| [PRIVACY_POLICY.md](PRIVACY_POLICY.md) / [TERMS_OF_SERVICE.md](TERMS_OF_SERVICE.md) | Юридические документы сервиса |

## Быстрый старт

```bash
# 1. Сборка
go build -ldflags "-s -w" -o build/xraytool .

# 2. Первый запуск создаст конфиг с дефолтами
./build/xraytool --config /etc/xraytool/config.yaml userlist

# 3. Отредактируйте /etc/xraytool/config.yaml (mode, server.api_key, server.domain, paths)

# 4. Сгенерируйте config.json для Xray-core и запустите API
./build/xraytool rebuild-config
./build/xraytool start-server
```

Подробности — в [deployment.md](deployment.md).
