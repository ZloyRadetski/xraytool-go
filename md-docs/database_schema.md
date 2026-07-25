# Схема базы данных

Слой данных — GORM (`internal/database`). Поддерживаются **SQLite** (по умолчанию) и
**PostgreSQL**. Модели описаны в `internal/database/models.go`, миграции — `autoMigrate`
в `internal/database/db.go`.

## Подключение

```yaml
database:
  driver: sqlite                       # или postgres
  sqlite_path: /etc/xraytool/xraytool.db
  dsn: "postgres://user:pass@host:5432/xraytool?sslmode=disable"
```

Особенности:

* для SQLite пул ограничен одним соединением (`SetMaxOpenConns(1)`) — это устраняет
  `database is locked` при конкурентной записи;
* для PostgreSQL дополнительно создаётся функциональный индекс
  `idx_users_telegram_id ON users ((metadata->>'telegram_id'))`;
* `AutoMigrate` выполняется автоматически в командах `start-server` (с `--run-migrations`),
  `migrate` и `db-migrate`.

## Таблицы

### `users`

| Поле | Тип | Примечание |
| --- | --- | --- |
| `id` | text, PK | UUID v4, генерируется приложением |
| `username` | text | Отображаемое имя |
| `balance` | int | Внутренний баланс (целые единицы) |
| `is_admin` | bool | Права администратора |
| `ref_code` | text, unique | Собственный реферальный код |
| `referred_by` | text, index, nullable | Кто пригласил (FK → `users.id`) |
| `metadata` | JSON | Платформенные данные: `telegram_id`, `telegram_username`, `email`, `source`, … |
| `is_blocked` | bool | Глобальная блокировка администратором |
| `created_at` | timestamp | |

### `subscriptions`

| Поле | Тип | Примечание |
| --- | --- | --- |
| `id` | text, PK | UUID v4 |
| `user_id` | text, index | FK → `users.id` |
| `email` | text, unique | Идентификатор клиента в Xray, например `bot_client_123456` |
| `xray_uuid` | text, unique | UUID клиента в инбаундах; он же `id` в ссылке подписки |
| `status` | text, index | `active`, `inactive`, `expired`, `blocked` |
| `max_devices` | int | Лимит одновременных устройств (по умолчанию 3) |
| `starts_at`, `ends_at` | timestamp, nullable | Окно действия, `ends_at` индексирован |
| `auto_renew` | bool | Автопродление за счёт баланса |
| `metadata` | JSON | План, промокод и прочее |
| `created_at`, `updated_at` | timestamp | |

### `devices`

| Поле | Тип | Примечание |
| --- | --- | --- |
| `id` | int64, PK | |
| `subscription_id` | text, index | FK → `subscriptions.id` |
| `hwid` | text, index | Отпечаток устройства от клиента |
| `device_model`, `device_os`, `user_agent` | text | Данные для отображения в кабинете |

### `payments`

| Поле | Тип | Примечание |
| --- | --- | --- |
| `id` | int64, PK | |
| `user_id` | text, index | FK → `users.id` |
| `amount` | int | Сумма в целых единицах |
| `status` | text | `pending_card`, `completed`, `canceled`, … |
| `payment_type` | text | `subscription`, `device_slot`, `balance`, … |
| `method` | text | `platega`, `balance`, `cash`, `sbp`, … |
| `external_id` | text, unique, nullable | Идентификатор транзакции провайдера (NULL для ручных платежей) |
| `custom_data` | JSON | Сырые поля ответа провайдера |
| `plan_id`, `promo_code_id` | int64, nullable | Ссылки на тариф и промокод |
| `created_at` | timestamp | |

### `plans`

`id`, `months` (unique), `base_price`, `global_discount_percent`, `is_active`,
`created_at`, `updated_at`.

### `promo_codes`

`id`, `code` (unique), `discount_percent`, `max_uses` (0 = без ограничений), `uses_count`,
`target_platform` (`all` / `bot` / `web`), `expires_at`, `is_active`, `created_at`,
`updated_at`.

### `referral_rewards`

`id`, `referrer_id` (index), `referred_id` (index), `payment_id` (unique — защита от
двойного начисления), `amount`, `created_at`.

### `subscription_notifications`

Составной первичный ключ (`subscription_id`, `warning_level`) + `sent_at`. Гарантирует,
что предупреждение об истечении каждого уровня (72h/24h/3h/1h) уходит один раз.

### `antifraud_bans`

`id`, `email` (unique), `banned_at`, `expires_at` (index), `reason`. Бан существует только
в рантайме Xray: запись в `config.json` не удаляется. См. [antifraud.md](antifraud.md).

### `sync_events`

Append-only журнал изменений VPN-пользователей: `id` (монотонный), `action`
(`add` / `update` / `remove`), `payload` (JSON `domain.VPNUserConfig`), `created_at`.
События старше 7 дней вычищаются.

### `sync_state`

Единственная строка `id = 1`: `last_event_id`, `state_hash`, `updated_at`. Ведётся и на
мастере, и на слейве. См. [cluster_sync.md](cluster_sync.md).

## Доступ к данным

Все запросы идут через порт `domain.Registry`:

```go
reg.Users(), reg.Subscriptions(), reg.Payments(), reg.Plans(), reg.Promos(),
reg.AntifraudBans(), reg.Devices(), reg.Notifications(), reg.SyncEvents()

// атомарные операции — единая транзакция на все репозитории
err := reg.WithTx(ctx, func(tx domain.Registry) error { ... })
```

Например, создание пользователя и запись события синхронизации выполняются в одной
транзакции, поэтому журнал не может «разъехаться» с состоянием подписок.

## Миграция с legacy-базы

```bash
xraytool db-migrate --from /путь/к/старой/bot.db
```

Переносятся пользователи, подписки и связанные данные из SQLite-базы старого Telegram-бота
(`internal/legacy`). Команда идемпотентна: записи, уже перенесённые ранее, пропускаются.

## Резервное копирование

```bash
# SQLite (безопасно на работающем сервере)
sqlite3 /etc/xraytool/xraytool.db ".backup '/root/backup-$(date +%F).db'"

# PostgreSQL
pg_dump "$DSN" | gzip > /root/xraytool-$(date +%F).sql.gz
```

Помимо базы, сохраняйте `config.yaml`, `xray_template.json`, `configs.txt` и файл
Reality-ключей — без них восстановление кластера потребует переоформления подписок.
