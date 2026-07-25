# Архитектура xraytool-go

## 1. Что это такое

`xraytool-go` — бэкенд управления кластером VPN-узлов на базе **Xray-core**. Одна и та же
программа работает в двух режимах (`mode` в `config.yaml`):

* **master** — хранит базу данных (пользователи, подписки, платежи, промокоды, баны),
  обслуживает REST API и выдачу подписок, рассылает состояние на подчинённые узлы;
* **slave** — не хранит бизнес-данные, принимает команды от мастера и применяет их к своему
  локальному Xray-core (добавление/удаление клиентов, ротация Reality-ключей).

Один бинарник совмещает три роли: CLI-утилита, HTTP-сервер и набор фоновых воркеров.

## 2. Слои и зависимости

Проект следует схеме **ports & adapters** (гексагональная архитектура):

```
              ┌───────────────────────────────────────────────┐
  CLI (cmd/)  │            Прикладной слой (use-cases)        │  HTTP (internal/server)
──────────────┤  internal/user, internal/payment,             ├────────────────────────
              │  internal/subscription, internal/statesync,    │
              │  internal/antifraud, internal/worker           │
              └───────────────┬───────────────┬───────────────┘
                              │ порты (интерфейсы) internal/domain
              ┌───────────────┴───────┐   ┌───┴─────────────────────┐
              │ Адаптер данных        │   │ Адаптер VPN-движка      │
              │ internal/database     │   │ internal/vpn (Xray-core)│
              │ (GORM: SQLite/Postgres│   │ gRPC API + config.json  │
              └───────────────────────┘   └─────────────────────────┘
```

* `internal/domain` — только типы и интерфейсы (`Registry`, `Engine`, `EventPropagator`,
  `ClusterStatsProvider`, `StateSyncSlaveProvider`, `FraudEventReporter`). Бизнес-логика
  зависит **только** от них.
* `internal/database` — реализация `domain.Registry` поверх GORM (Unit of Work + репозитории,
  транзакции через `WithTx`).
* `internal/vpn` — единственное место, где допускается импорт типов Xray-core. Реализует
  `domain.Engine`: правка `config.json`, hot-add/hot-remove по gRPC, статистика трафика,
  ротация логов, Reality-ключи.
* `internal/mocks` — сгенерированные моки портов для тестов.

Композиционный корень — `cmd/root.go` (`loadDependencies`) и `cmd/server.go`: именно там
выбирается конкретный движок (`engine.type`, по умолчанию `xray`) и собираются все сервисы.
Добавление нового ядра (sing-box, mihomo) сводится к новому адаптеру `domain.Engine` и одной
ветке `switch` в `cmd/server.go`.

## 3. Карта пакетов

| Пакет | Назначение |
| --- | --- |
| `cmd` | CLI на cobra: команды, флаги, загрузка зависимостей |
| `internal/appconfig` | Загрузка/валидация `config.yaml`, значения по умолчанию, генерация конфига при первом запуске |
| `internal/domain` | Порты (интерфейсы) и доменные модели |
| `internal/database` | GORM-репозитории, модели, авто-миграции |
| `internal/user` | Сценарии работы с пользователями: создание, блокировка, лимиты, срок действия |
| `internal/subscription` | Конвейер выдачи подписки, кеш шаблонов и конфига Xray |
| `internal/payment` | Платежи, планы, промокоды, реферальные начисления; провайдер Platega |
| `internal/vpn` | Адаптер Xray-core: config.json, gRPC-клиент, шаблоны, Reality-ключи |
| `internal/statesync` | Журнал событий и синхронизация Master → Slave |
| `internal/slave` | HTTP-клиент к Slave-нодам, реестр серверов, провайдеры синхронизации и статистики |
| `internal/antifraud` | Антифрод: ротация и чтение access-лога, анализатор IP, софт-баны |
| `internal/worker` | Фоновые воркеры: истечение подписок, синхронизация состояний, «скруббер» приватности |
| `internal/events` | Диспетчер исходящих вебхуков (HMAC-подпись, ретраи) |
| `internal/stats` | Накопительная статистика трафика по пользователям |
| `internal/convert` | Конвертация Xray JSON ⇄ share-ссылки (`vless://`, `vmess://`, `trojan://`, `ss://`) |
| `internal/mailer` | Отправка транзакционных писем через Resend |
| `internal/generate` | Криптостойкая генерация UUID, имён sub-файлов, секретов |
| `internal/safeio` | Атомарная запись файлов (temp + rename) |
| `internal/logger` | Инициализация `log/slog` (console/json, файл/stdout) |
| `internal/legacy` | Миграция данных из старой SQLite-базы Telegram-бота |

## 4. Основные потоки

### 4.1. Создание пользователя

```
CLI newuser / POST /api/v1/users/register
        │
        ▼
  user.Service.CreateUser
        │  ├── Registry.WithTx: создать User + Subscription (+ SyncEvent на мастере)
        │  ├── Engine.AddUser: правка config.json + hot-add по gRPC (без рестарта Xray)
        │  └── EventPropagator.PropagateAll("newuser") → Slave-ноды
        ▼
  ответ со ссылкой на подписку https://<domain>/client?id=<xray_uuid>
```

На мастере движок обёрнут в `statesync.EventAwareEngine`: любая мутация автоматически
пишется в журнал `sync_events`, откуда её забирают слейвы (см. [cluster_sync.md](cluster_sync.md)).

### 4.2. Выдача подписки

`GET /client`, `GET /api/v1/sub`, `GET /api/v2/sub` → `subscription.ProcessSQL`: проверка
User-Agent, поиск подписки по `xray_uuid`, проверка антифрод-бана, учёт устройства по HWID,
подстановка Reality-ключей и параметров в шаблон, отдача JSON-конфига или share-ссылок.
Подробности — в [subscriptions.md](subscriptions.md).

### 4.3. Фоновые процессы (`start-server`)

| Воркер | Интервал | Что делает |
| --- | --- | --- |
| `ExpiryWorker` | `worker.expiry_interval` (5m) | Находит истёкшие подписки, снимает пользователей с движка, шлёт вебхуки `subscription.expired` / `subscription.expiring`, удаляет лишние устройства |
| `SyncStatesWorker` | `worker.sync_states_interval` (3m) | На мастере: синхронизация состояния со всеми слейвами |
| `ScrubberWorker` | 24 часа | Очистка «цифрового следа» платежей (`external_id`) |
| Антифрод (5 горутин) | постоянно | Ротатор лога, tailer, анализатор, чистка IP-состояния, снятие банов |

Все воркеры завершаются по `SIGTERM`/`SIGINT`: HTTP-сервер получает 30 секунд на graceful
shutdown, затем дожидаются доставки вебхуков.

## 5. Топология кластера

```
                          ┌──────────────────────────┐
                          │  Master                  │
   клиенты/бот  ─────────►│  REST API + подписки     │
                          │  БД (SQLite/Postgres)    │
                          │  журнал sync_events      │
                          └────────┬─────────────────┘
                                   │ HTTP + X-API-Key
              ┌────────────────────┼────────────────────┐
              ▼                    ▼                    ▼
      ┌───────────────┐    ┌───────────────┐    ┌───────────────┐
      │ Slave #1      │    │ Slave #2      │    │ Slave #N      │
      │ xraytool+Xray │    │ xraytool+Xray │    │ xraytool+Xray │
      └───────────────┘    └───────────────┘    └───────────────┘
```

* Мастер знает слейвы из секции `slave_servers` в `config.yaml`.
* Слейв знает мастера из секции `master_api` (нужно для полной синхронизации и отправки
  антифрод-событий).
* Аутентификация — заголовок `X-API-Key` (сравнение в постоянном времени, `crypto/subtle`).
  Мастер принимает как собственный `server.api_key`, так и любой ключ из `slave_servers`.

> **Требование:** системное время на всех узлах должно быть синхронизировано (chrony /
> systemd-timesyncd). Расхождения ломают проверку сроков подписок и хеш-сравнение состояний.

## 6. Хранилище и файлы на диске

| Путь (по умолчанию) | Что это |
| --- | --- |
| `/etc/xraytool/config.yaml` | Конфигурация xraytool |
| `/etc/xraytool/xraytool.db` | База SQLite (при `database.driver: sqlite`) |
| `/etc/xraytool/xray_template.json` | Шаблон конфига Xray-core (скелет + статические клиенты) |
| `/usr/local/etc/xray/config.json` | Рабочий конфиг Xray-core, генерируется из шаблона + БД |
| `/usr/local/etc/xray/config.json.dirty` | Маркер «состояние на диске разошлось с рантаймом» — движок сам перезапустит Xray |
| `/etc/xraytool/configs.txt` | Шаблон подписки (JSON-конфиги клиента с плейсхолдерами) |
| `/etc/xraytool/routing.json`, `routing_ALL_RU.json` | Блоки маршрутизации, подставляемые в подписку |
| `/etc/xraytool/configs/reality.keys` | Пара X25519-ключей и пул Short ID |
| `/etc/xraytool/traffic_stats_state.json` | Накопительная статистика трафика |
| `/etc/xraytool/inferred_traffic.json` | Суммарная статистика по кластеру (master + slaves) |
| `/dev/shm/xray-access.log` | Access-лог Xray для антифрода (обязательно в tmpfs) |

## 7. Безопасность

* Все защищённые маршруты требуют `X-API-Key`; неверные запросы логируются как `INTRUDER`
  с маскированием секретных заголовков.
* Вебхук Platega проверяется по merchant ID и секрету; исходящие вебхуки подписываются
  HMAC-SHA256 (`X-Webhook-Signature`).
* `/api/rest/upload` и `/api/rest/download` ограничены белым списком каталогов
  (`server.allowed_dirs`, проверка после раскрытия симлинков).
* API-сервер слушает `127.0.0.1` — наружу его следует публиковать через reverse-proxy с TLS.
* Файлы ключей и конфигов пишутся с правами `0600`/`0644` атомарно (`internal/safeio`).
