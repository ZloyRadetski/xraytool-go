# Справочник CLI

Все команды принимают глобальный флаг `--config` (по умолчанию `/etc/xraytool/config.yaml`).
Перед выполнением любой команды загружается конфиг, инициализируется логгер, подключение к БД
и адаптер VPN-движка; после выполнения соединения корректно закрываются.

Большинство команд, изменяющих состояние, требуют **root** (на Windows проверка пропускается).

В Docker-развертывании удобно завести обёртку:

```bash
echo '#!/bin/bash'                                   | sudo tee    /usr/local/bin/xraytool
echo 'docker exec -it xraytool_backend ./xraytool "$@"' | sudo tee -a /usr/local/bin/xraytool
sudo chmod +x /usr/local/bin/xraytool
```

## Обзор

| Команда | Назначение |
| --- | --- |
| `start-server` | Запуск REST API и фоновых воркеров |
| `newuser` | Создать пользователя |
| `rmuser` | Удалить пользователя навсегда |
| `limit` / `unlimit` | Заблокировать / разблокировать пользователя |
| `setexpire` | Изменить дату окончания подписки |
| `setlimit` (`updatelimit`, `set-limit`) | Изменить лимит устройств |
| `userlist` | Список активных и заблокированных пользователей |
| `sharelink` | Показать ссылку на подписку пользователя |
| `cli-stats` | Статистика трафика по пользователям |
| `ips` | Снимок активных IP из работающего модуля антифрода |
| `apply-batch` | Пакетное применение операций над пользователями |
| `rebuild-config` | Пересобрать `config.json` из шаблона и БД |
| `rotate-keys` | Принудительная ротация Reality-ключей и Short ID |
| `syncstates` | Синхронизировать состояние со всеми слейвами |
| `sync-xray` | Привести UUID клиентов в движке в соответствие с БД |
| `migrate` | Очистить устаревшие поля конфига и пересинхронизировать пользователей |
| `db-migrate` | Импорт данных из legacy-базы Telegram-бота |
| `convert` | Конвертация Xray JSON ⇄ share-ссылки |
| `genbalancer` | Сгенерировать конфиг-балансировщик из подписки |
| `update-xray` / `update-geo` | Обновить xray-core / базы GeoIP и Geosite |
| `completion` | Скрипты автодополнения оболочки |

---

## Сервер

### `start-server`

Запускает REST API (см. [api_documentation.md](api_documentation.md)) и фоновые воркеры.

| Флаг | По умолчанию | Описание |
| --- | --- | --- |
| `--port` | `8080` (или `ports.api_server`) | Порт прослушивания на `127.0.0.1` |
| `--run-migrations` | `false` | Выполнить AutoMigrate при старте |
| `--api-config` | `xray_api_config.json` | Путь к JSON конфигурации API |

Что происходит при старте: проверка `server.api_key`, подключение к БД, сборка движка,
первичная синхронизация пользователей из БД в работающий Xray (без рестарта), инициализация
кеша подписок, запуск антифрода и воркеров, подписка на `SIGTERM`/`SIGINT`.

---

## Пользователи

### `newuser`

```bash
xraytool newuser --email bot_client_12345 --expire 31-12-2026 --limit 3
```

| Флаг | Описание |
| --- | --- |
| `--email` (`--name`) | Идентификатор пользователя (email в терминах Xray) |
| `--expire` | Дата окончания в формате `DD-MM-YYYY` (по умолчанию +30 дней) |
| `--limit` | Лимит одновременных устройств |
| `--legacy` | Старый режим: правка конфига и рестарт Xray вместо hot-add |

Без флагов команда работает в интерактивном режиме.

### `rmuser`, `limit`, `unlimit`

```bash
xraytool rmuser  --email bot_client_12345
xraytool limit   --email bot_client_12345
xraytool unlimit --email bot_client_12345 --expire 31-12-2026 --limit 3
```

* `rmuser` — полное удаление пользователя;
* `limit` — блокировка: пользователь снимается с движка и помечается в БД;
* `unlimit` — разблокировка с возможностью задать новый срок и лимит устройств.

Общий флаг `--legacy` включает правку конфига с рестартом ядра.

### `setexpire`, `setlimit`

```bash
xraytool setexpire --email bot_client_12345 --expire 01-03-2027
xraytool setlimit  --email bot_client_12345 --limit 5
```

### `userlist`, `sharelink`

```bash
xraytool userlist            # человекочитаемая таблица
xraytool userlist --batch    # машиночитаемый вывод
xraytool sharelink --email bot_client_12345
```

### `apply-batch`

Пакетное применение операций одним вызовом — полезно для массовых импортов.

```bash
xraytool apply-batch --file /etc/xraytool/batch.json
cat batch.json | xraytool apply-batch --file -
xraytool apply-batch --payload '{"add":[{"email":"a@b","uuid":"...","expire":"31-12-2026"}],"remove":["old@b"]}'
```

| Флаг | Описание |
| --- | --- |
| `--file` | Путь к файлу с JSON-полезной нагрузкой (`-` — читать stdin) |
| `--payload` | JSON-строка с операциями (можно передать и позиционным аргументом) |

Формат полезной нагрузки:

```json
{
  "add": [
    {"email": "bot_client_1", "uuid": "…", "auth": "…", "subfile": "abc.txt", "expire": "31-12-2026", "limit": 3}
  ],
  "remove": ["bot_client_2"]
}
```

---

## Статистика и диагностика

### `cli-stats`

```bash
xraytool cli-stats                      # таблица по всем пользователям
xraytool cli-stats --email bot_client_1 # один пользователь
xraytool cli-stats --api                # JSON
xraytool cli-stats --inferred           # сводные данные по кластеру
```

### `ips`

Показывает живой снимок отслеживаемых антифродом IP-адресов (запрашивается у работающего
процесса `start-server`). Требует включённого модуля антифрода.

---

## Конфигурация Xray и ключи

### `rebuild-config`

Пересобирает `config.json` из шаблона и активных пользователей БД, подставляя актуальные
Reality-ключи и Short ID.

| Флаг | Описание |
| --- | --- |
| `--sync` | После пересборки запустить синхронизацию всех слейвов |

### `rotate-keys`

Генерирует новую пару X25519 и 15 новых Short ID, сохраняет их в `reality.keys_filepath`,
перестраивает локальный конфиг и рассылает ключи слейвам (действием `sync-keys`).
Требует `reality.rotation_enabled: true`.

### `sync-xray`

Приводит UUID существующих клиентов движка в соответствие с `xray_uuid` из БД. Ничего не
удаляет — только правит совпадающих по email клиентов.

### `migrate`

Чистит устаревшие поля конфига и синхронизирует всех пользователей с текущими шаблонами.
Флаг `--legacy` дополнительно перезапускает движок.

---

## Кластер

### `syncstates`

```bash
xraytool syncstates             # инкрементальная синхронизация (delta по хешу)
xraytool syncstates --full      # принудительная полная синхронизация
xraytool syncstates --dry-run   # показать изменения без применения
```

Выполняется только на мастере. Протокол описан в [cluster_sync.md](cluster_sync.md).

---

## Миграция и конвертация

### `db-migrate`

```bash
xraytool db-migrate --from /etc/xraytool/bot.db
```

Переносит пользователей и подписки из старой SQLite-базы Telegram-бота в текущую БД.
Повторный запуск безопасен: уже перенесённые записи (по Telegram ID в metadata) пропускаются.

### `convert`

```bash
xraytool convert /etc/xraytool/config.json        # JSON → share-ссылки
xraytool convert -i "vless://uuid@host:443?..."    # ссылка → Xray JSON
cat config.json | xraytool convert -               # чтение из stdin
```

### `genbalancer`

Скачивает файл подписки, разбирает ссылки `vless://`, `vmess://`, `trojan://`, `ss://`,
превращает каждую в outbound с тегами `AT_001..AT_NNN` и печатает готовый конфиг
балансировщика.

| Флаг | По умолчанию | Описание |
| --- | --- | --- |
| `-u, --url` | подписка с публичным списком | Источник ссылок |
| `-o, --output` | stdout | Файл результата |
| `--remarks` | `🇪🇺 БАЛАНСЕР` | Значение поля `remarks` |
| `-s, --upsert-into` | — | Вставить/заменить конфиг в JSON-массиве подписки (совпадение по `remarks`) |
| `--upsert-sub` | — | То же, но путь берётся из `paths.json_subscription_template` |

---

## Обслуживание системы

```bash
xraytool update-xray   # обновление xray-core до последней версии
xraytool update-geo    # обновление geoip.dat и geosite.dat
```
