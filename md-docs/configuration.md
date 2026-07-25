# Справочник конфигурации (`config.yaml`)

Путь к файлу задаётся глобальным флагом `--config` (по умолчанию `/etc/xraytool/config.yaml`).
Если файла нет, при первом запуске он создаётся с полным набором закомментированных примеров
и правами `0600`. Отсутствующие поля добираются значениями по умолчанию, после чего конфиг
валидируется (`appconfig.Load` → `Config.Validate`).

Реализация: `internal/appconfig/config.go`.

> Переменные окружения не читаются: единственный источник настроек — YAML-файл
> (закомментированный `XRAYTOOL_DB_DSN` в `docker-compose.yml` — задел на будущее).

## Валидация

Для `mode: master` обязательны:

* `server.domain` — иначе ошибка `server.domain is required for master nodes`;
* `database.dsn` — если `database.driver` не `sqlite`.

Дополнительно `start-server` откажется стартовать, если `server.api_key` пуст или равен
`CHANGE_ME_IN_CONFIG`.

## Корневые параметры

| Ключ | Тип | По умолчанию | Описание |
| --- | --- | --- | --- |
| `mode` | string | `master` | Роль узла: `master` или `slave` |
| `platega_merchant_id` | string | — | Merchant ID платёжного провайдера Platega |
| `platega_secret` | string | — | Секрет Platega: подпись исходящих запросов и проверка колбэка |
| `webhook_secret` | string | — | Ключ HMAC-SHA256 для подписи исходящих вебхуков |
| `webhooks` | []string | `[]` | Список URL, получающих события (см. [webhooks.md](webhooks.md)) |
| `blacklisted_admins` | []string | `[]` | Клиенты из шаблона, которых нельзя синхронизировать в рабочий `config.json` |

## `engine`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `engine.type` | `xray` | Тип VPN-ядра. Пустая строка и `xray` дают адаптер Xray-core; другие значения зарезервированы под будущие ядра |

## `server`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `server.ip` | — | Публичный IP узла; подставляется в подписку как `{HOST}` |
| `server.domain` | `yourdomain.tld` | Домен ссылок подписки `https://<domain>/client?id=<uuid>` |
| `server.api_key` | `CHANGE_ME_IN_CONFIG` | Ключ для **входящих** запросов (заголовок `X-API-Key`) |
| `server.allowed_dirs` | `/etc/xraytool`, `/var/www/TorvaldsVPN`, `/var/log/xray` | Белый список каталогов для `/api/rest/upload` и `/api/rest/download` |

## `ports`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `ports.api_server` | `8080` | Порт REST API. Сервер слушает только `127.0.0.1`; флаг `--port` имеет приоритет |

## `paths`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `xray_config` | `/usr/local/etc/xray/config.json` | Рабочий конфиг Xray-core |
| `xray_template` | `/etc/xraytool/xray_template.json` | Шаблон конфига (скелет inbounds + статические клиенты). Пустая строка отключает генерацию из шаблона |
| `stats_state` | `/etc/xraytool/traffic_stats_state.json` | Накопительная статистика трафика локального узла |
| `inferred_stats` | `/etc/xraytool/inferred_traffic.json` | Сводная статистика по кластеру |
| `json_subscription_template` | `/etc/xraytool/configs.txt` | Шаблон подписки (устаревший ключ `subscription_template` поддерживается) |
| `vless_subscription_template` | — | **Устарел**: VLESS-подписка генерируется из JSON-подписки на лету |
| `routing_template` | `/etc/xraytool/routing.json` | Блок маршрутизации для плейсхолдера `{GLOBAL_ROUTING}` |
| `routing_ru_template` | `/etc/xraytool/routing_ALL_RU.json` | Блок маршрутизации для `{RU_ROUTING}` |
| `hy2_config_yaml` | `/etc/hysteria/config.yaml` | Конфиг Hysteria 2 (используется для obfs-пароля) |
| `geoip_dat` | `/usr/local/share/xray/geoip.dat` | База GeoIP (обновляется командой `update-geo`) |
| `geosite_dat` | `/usr/local/share/xray/geosite.dat` | База Geosite |

## `xray`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `xray.api_addr` | `127.0.0.1:10085` | Адрес gRPC API Xray-core (должен совпадать с секцией `api` в `config.json`) |

## `database`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `database.driver` | `sqlite` | `sqlite` или `postgres` |
| `database.dsn` | — | DSN Postgres, например `postgres://user:pass@localhost:5432/xraytool?sslmode=disable` |
| `database.sqlite_path` | `/etc/xraytool/xraytool.db` | Путь к файлу SQLite |

Авто-миграции выполняются только для команд `start-server`, `migrate` и `db-migrate`.

## `stats`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `stats.bucket_seconds` | `60` | Гранулярность корзин трафика |
| `stats.detailed_retention_days` | `2` | Сколько дней хранить детальные корзины перед агрегацией |

## `slave_api` (используется мастером)

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `slave_api.connect_timeout` | `5s` | Таймаут установки соединения со слейвом |
| `slave_api.request_timeout` | `30s` | Общий таймаут запроса |
| `slave_api.remote_path` | `/api/v1/internal/xray/sync` | Путь на слейве по умолчанию (в примере конфига — `/api/rest/xraytool`; можно переопределить полным URL в `slave_servers`) |

## `slave_servers` (только мастер)

Словарь `имя → параметры`:

```yaml
slave_servers:
  slave-1:
    url: "https://slave1.example.com/api/v1/internal/xray/sync"
    api_key: "unique_secret_for_slave_1"   # уходит слейву в заголовке X-API-Key
    insecure: false                        # true — не проверять TLS-сертификат
```

Все перечисленные `api_key` также принимаются входящим middleware мастера — это позволяет
слейвам обращаться к его endpoint'ам снапшота и состояния.

## `master_api` (только слейв)

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `master_api.url` | — | URL sync-эндпоинта мастера. Слейв сам выводит из него путь `/snapshot` при полной синхронизации |
| `master_api.api_key` | `CHANGE_ME_IN_CONFIG` | Ключ для обращений **к** мастеру |
| `master_api.insecure` | `false` | Игнорировать самоподписанные сертификаты |

## `worker`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `worker.enabled` | `true` | Включает фоновые воркеры |
| `worker.expiry_interval` | `5m` | Период проверки истечения подписок и лимитов устройств |
| `worker.sync_states_interval` | `3m` | Период синхронизации мастера со слейвами |
| `worker.expiration_warnings` | `72h, 24h, 3h, 1h` | Пороги предупреждений `subscription.expiring` (каждый порог срабатывает один раз на подписку) |

## `logging`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `logging.level` | `info` | `debug`, `info`, `warn`, `error` |
| `logging.file_path` | — | Файл лога; пусто — только stdout |
| `logging.format` | `console` | `console` (разработка) или `json` (прод) |

## `reality`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `reality.rotation_enabled` | `false` | Генерация и ротация Reality-ключей. Включать **только** на мастере |
| `reality.keys_filepath` | `/etc/xraytool/configs/reality.keys` | JSON с приватным/публичным ключом X25519 и 15 Short ID |

## `subscription`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `subscription.user_agent_whitelist` | `happ`, `incy`, `megasupersecretua`, `v2ray` | Только эти User-Agent получают подписку, остальные — `403` |
| `subscription.user_agent_no_checks` | `megasupersecretua`, `v2ray` | Клиенты, для которых пропускаются проверки HWID и лимита устройств |
| `subscription.dummy_configs.expired` | текст-заглушка | Строки «профиля-заглушки» для истёкшей подписки |
| `subscription.dummy_configs.device_limit` | текст-заглушка | Заглушка при превышении лимита устройств |
| `subscription.dummy_configs.unsupported_client` | текст-заглушка | Заглушка, если клиент не прислал HWID |
| `subscription.dummy_configs.anti_fraud` | текст-заглушка | Заглушка во время антифрод-бана |

## `anti_fraud`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `anti_fraud.enabled` | `false` | Включает модуль антифрода |
| `anti_fraud.dry_run` | `true` | Только детектирование и логирование, без применения банов |
| `anti_fraud.log_path` | `/dev/shm/xray-access.log` | Access-лог Xray; **обязательно** в RAM-ФС |
| `anti_fraud.max_ips` | `3` | Базовый лимит уникальных IP на пользователя в окне; фактический лимит умножается на число разрешённых устройств |
| `anti_fraud.ip_limit_ttl` | `3m` | Скользящее окно учёта IP |
| `anti_fraud.ban_duration` | `10m` | Длительность софт-бана |
| `anti_fraud.log_rotation_size_mb` | `20` | Порог ротации access-лога по размеру |
| `anti_fraud.log_rotation_max_age` | `5m` | Порог ротации по возрасту файла |
| `anti_fraud.report_to_master` | `false` | На слейве: отправлять пачки IP-событий мастеру каждые 5 секунд |
| `anti_fraud.salt_secret` | — | Общая соль хеширования IP. Если не задана, используется общекластерная константа |

## `mailer`

| Ключ | По умолчанию | Описание |
| --- | --- | --- |
| `mailer.enabled` | `false` | Включает отправку писем (OTP-коды) через Resend |
| `mailer.resend_api_key` | — | Ключ API Resend (`re_...`) |
| `mailer.from_email` | — | Подтверждённый адрес отправителя |

## Минимальный пример: мастер

```yaml
mode: master
server:
  ip: "203.0.113.10"
  domain: "vpn.example.com"
  api_key: "long_random_master_key"
database:
  driver: sqlite
  sqlite_path: /etc/xraytool/xraytool.db
paths:
  xray_config: /usr/local/etc/xray/config.json
  xray_template: /etc/xraytool/xray_template.json
reality:
  rotation_enabled: true
slave_servers:
  slave-de:
    url: "https://de.example.com/api/v1/internal/xray/sync"
    api_key: "long_random_slave_de_key"
```

## Минимальный пример: слейв

```yaml
mode: slave
server:
  ip: "203.0.113.20"
  domain: "de.example.com"
  api_key: "long_random_slave_de_key"   # тот же ключ, что мастер шлёт этому слейву
master_api:
  url: "https://vpn.example.com/api/v1/internal/xray/sync"
  api_key: "long_random_master_key"
reality:
  rotation_enabled: false
anti_fraud:
  enabled: true
  report_to_master: true
  salt_secret: "shared_cluster_salt"
```
