# Выдача подписок

Пакет `internal/subscription` (основная функция `ProcessSQL`), HTTP-слой —
`internal/server/handlers_sub.go`.

## 1. Маршруты

| Маршрут | Аутентификация | Описание |
| --- | --- | --- |
| `GET /client` | нет | Публичная ссылка, которую получает пользователь |
| `GET /api/v1/sub` | нет | Синоним `/client` |
| `GET /api/v2/sub` | нет | Тот же обработчик; оставлен для совместимости с ботом |

Идентификатор передаётся в query: `?id=<xray_uuid>` (принимаются также `sub`, `token` и
имя sub-файла — см. `ResolveClientID`).

Формат ответа:

* по умолчанию — готовый **JSON-конфиг** Xray (`Content-Disposition: config.json`);
* при `?format=vless` — текстовый список share-ссылок (`configs.txt`).

## 2. Конвейер обработки

```
1. Определить client id из query/пути                 → 404 «Invalid client id»
2. Проверить User-Agent по белому списку              → 403 «Unsupported client user-agent»
3. Найти подписку в БД по xray_uuid
   └── не найдена → искать клиента в config.json и в шаблоне (админский fallback)
4. Проверить антифрод-бан                             → заглушка, X-Reject-Reason: antifraud_ban
5. Учесть устройство по HWID (если проверки не отключены)
   ├── HWID отсутствует         → заглушка, X-Reject-Reason: unsupported_client
   └── лимит устройств исчерпан → заглушка, X-Reject-Reason: device_limit_reached
                                  + вебхук device.limit_reached
6. Подписка истекла/заблокирована → заглушка, X-Reject-Reason: blocked_or_expired
7. Подставить параметры в шаблон, отдать JSON или share-ссылки
```

Во всех «отказных» случаях возвращается **200 OK** с профилем-заглушкой: так клиентские
приложения показывают пользователю понятный текст вместо ошибки сети.

## 3. User-Agent

* `subscription.user_agent_whitelist` — только эти клиенты вообще получают ответ.
* `subscription.user_agent_no_checks` — сервисные клиенты (например, бот), для которых
  пропускаются проверки HWID и лимита устройств; их запросы логируются на уровне `debug`.

## 4. Учёт устройств (HWID)

HWID берётся из первого найденного значения:

* query: `hwid`, `device_id`, `deviceid`, `deviceId`, `udid`, …
* заголовки: `HWID`, `X-HWID`, `X-Device-Id`, `Device-Id`, `X-UDID`, …

Дополнительно собираются `device_model` и `device_os`. `Devices.TrackDevice` создаёт или
обновляет запись устройства и возвращает признак превышения лимита `subscriptions.max_devices`
(по умолчанию 3). Лишние устройства (самые старые по `last_seen`) удаляет `ExpiryWorker`.

## 5. Заголовки ответа

| Заголовок | Значение |
| --- | --- |
| `Profile-Title` | Название профиля из шапки шаблона подписки |
| `Subscription-Userinfo` | `upload=…; download=…; total=…; expire=<unix>` |
| `Profile-Update-Interval` | Интервал обновления в часах (по умолчанию `12`) |
| `Profile-Web-Page-Url` | Ссылка на личный кабинет (если задана в шапке) |
| `Profile-Type` | `Sip002` |
| `X-Sub-Source` | `database` или `xray config` (админский fallback) |
| `X-Checks-Bypass` | `none`, `ua:<agent>`, `is-user-blocked`, `admin-fallback` |
| `X-Is-User-Blocked` | `true` / `false` |
| `X-Reject-Reason` | Причина выдачи заглушки, если она была |

Ответ всегда помечается `Cache-Control: no-store` и `Pragma: no-cache`.

Данные о трафике берутся сначала из `paths.inferred_stats` (сумма по кластеру), при
отсутствии пользователя — из локального `paths.stats_state`.

## 6. Шаблон подписки (`configs.txt`)

Файл состоит из необязательной шапки с метаданными и одного или нескольких JSON-конфигов.
В тексте конфигов заменяются плейсхолдеры:

| Плейсхолдер | Значение |
| --- | --- |
| `{HOST}` | `server.ip` |
| `{SNI}` | SNI первого Reality-инбаунда |
| `{PBK}` | Публичный Reality-ключ |
| `{SID}` | Short ID, детерминированно выбранный для подписки: `sha256(subscription_id) mod N` |
| `{UUID}` | `xray_uuid` пользователя |
| `{EMAIL}` | Email (идентификатор) пользователя |
| `{SS_AUTH}` | base64 от `2022-blake3-aes-256-gcm:<серверный пароль>:<пароль пользователя>` |
| `{HY2_AUTH}`, `{HYSTERIA2_AUTH}` | Пароль Hysteria 2 (детерминированно выводится из UUID+email, если не задан) |
| `{HY2_OBFS}`, `{HY2_OBFS_PASSWORD}`, `{HYSTERIA2_OBFS}` | obfs-пароль Hysteria 2 из `paths.hy2_config_yaml` |
| `{GLOBAL_ROUTING}` | Содержимое `paths.routing_template` |
| `{RU_ROUTING}` | Содержимое `paths.routing_ru_template` |
| `{UP}`, `{DOWN}` | Отданный/принятый трафик в байтах |

Результат проверяется на валидность JSON; при ошибке возвращается `500` — то есть битый
шаблон не «уедет» клиенту.

## 7. Кеш

`subscription.CacheManager` держит в памяти рабочий `config.json`, шаблон подписки, оба
блока маршрутизации и Reality-ключи; `Refresh()` перечитывает файлы по mtime. Это убирает
дисковые операции с горячего пути выдачи подписки.

## 8. Share-ссылки

Конвертация JSON → `vless://`, `vmess://`, `trojan://`, `ss://` живёт в `internal/convert`
и используется как в `?format=vless`, так и в командах `sharelink`, `convert`, `genbalancer`.

## 9. Диагностика

```bash
curl -s -D - -A "happ/1.0" "https://vpn.example.com/client?id=<uuid>&hwid=test-device" -o /dev/null
```

Смотрите заголовки `X-Sub-Source`, `X-Checks-Bypass`, `X-Reject-Reason` — они однозначно
объясняют, почему клиент получил именно такой ответ.
