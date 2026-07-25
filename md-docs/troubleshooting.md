# Диагностика проблем

## Как быстро понять, что происходит

```bash
journalctl -u xraytool -f              # systemd
docker compose logs -f backend         # docker
journalctl -u xray -n 200              # логи ядра
xraytool userlist                      # состояние пользователей
xraytool syncstates --dry-run          # расхождения с слейвами
xraytool ips                           # состояние антифрода
```

Уровень логов повышается через `logging.level: debug`.

---

## Сервер не стартует

| Сообщение | Причина и решение |
| --- | --- |
| `server.api_key is not configured` / `CHANGE_ME_IN_CONFIG` | Задайте `server.api_key` в `config.yaml` |
| `server.domain is required for master nodes` | Заполните `server.domain` |
| `database.dsn is required when driver is not sqlite` | Укажите DSN Postgres или переключитесь на sqlite |
| `must be run as root` | Запустите с правами root (нужен доступ к конфигам Xray и systemd) |
| `address already in use` | Порт занят: `ss -lptn 'sport = :8080'`, смените `ports.api_server` или `--port` |

## `401 unauthorized` в API

* Проверьте заголовок: именно `X-API-Key`, а не `Authorization`.
* Значение должно совпадать с `server.api_key` узла (мастер также принимает ключи из
  `slave_servers`).
* В логах такие запросы видны как `INTRUDER` с указанием IP.

## Подписка не выдаётся

Смотрите заголовки ответа — они прямо называют причину:

| Симптом | Причина |
| --- | --- |
| `403 Unsupported client user-agent` | UA клиента отсутствует в `subscription.user_agent_whitelist` |
| `404 Invalid client id` / `User not found` | Неверный `?id=` или подписки нет в БД |
| `X-Reject-Reason: unsupported_client` | Клиент не прислал HWID; добавьте его UA в `user_agent_no_checks` или используйте клиент с поддержкой HWID |
| `X-Reject-Reason: device_limit_reached` | Исчерпан `max_devices`; увеличьте лимит или удалите устройство через API |
| `X-Reject-Reason: blocked_or_expired` | Подписка истекла или заблокирована |
| `X-Reject-Reason: antifraud_ban` | Активен софт-бан антифрода |
| `500 subscription template not found in cache` | Нет файла `paths.json_subscription_template` |
| `500 Invalid template config JSON` | Шаблон стал невалидным JSON после подстановки плейсхолдеров |

## Пользователь добавлен, но не может подключиться

1. `xraytool cli-stats --email <email>` — виден ли пользователь ядру.
2. Проверьте, что gRPC API доступен: `ss -lptn 'sport = :10085'`, значение `xray.api_addr`.
3. Файл `config.json.dirty` рядом с конфигом означает расхождение диска и рантайма —
   выполните `xraytool rebuild-config` и перезапустите Xray.
4. `xraytool sync-xray` приведёт UUID клиентов в соответствие с БД.

## Слейв не синхронизируется

1. `xraytool syncstates --dry-run` на мастере — что отвечает узел.
2. Проверьте доступность и ключ:
   ```bash
   curl -s -o /dev/null -w '%{http_code}\n' -H "X-API-Key: $SLAVE_KEY" \
     -X POST https://slave/api/v1/internal/xray/sync -d '{"action":"sync-ping"}'
   ```
3. Ошибка TLS на самоподписанном сертификате — `slave_servers.<node>.insecure: true`
   (лучше — установить нормальный сертификат).
4. Если ping всегда `match=false`: сверьте системное время, убедитесь, что конфиг слейва
   не правился вручную, и выполните `xraytool syncstates --full`.
5. Слейв должен уметь ходить **к мастеру** (`master_api.url`) — full-sync тянет снапшот
   именно слейвом.

## Антифрод не срабатывает

* `anti_fraud.enabled: true` и `dry_run: false`;
* в `config.json` Xray включён access-лог в тот же путь, что `anti_fraud.log_path`;
* файл существует и растёт: `tail -f /dev/shm/xray-access.log`;
* помните про динамический порог `max_ips × max_devices`;
* на слейвах с `report_to_master: true` баны выносит **только мастер**;
* `salt_secret` должен быть одинаков на всех узлах.

## Ложные баны антифрода

Увеличьте `max_ips` (мобильные сети и CGNAT дают частую смену адресов) или уменьшите
`ban_duration`. Немедленное снятие: `POST /api/v1/admin/users/{email}/unblock` либо
`xraytool unlimit --email <email>`.

## `database is locked` (SQLite)

Пул уже ограничен одним соединением. Если ошибка сохраняется — значит, к базе обращается
второй процесс (например, ручной `sqlite3` в режиме записи или второй экземпляр xraytool).
Для больших нагрузок переходите на PostgreSQL.

## Вебхуки не доходят

* Список `webhooks` не пуст, URL доступен с сервера;
* приёмник отвечает `2xx` быстрее 10 секунд (иначе таймаут и до 4 попыток: 2с/4с/8с);
* при заданном `webhook_secret` получатель обязан корректно проверять
  `X-Webhook-Signature`;
* ищите в логах `[EVENT_DISPATCHER]`.

## Reality: клиенты перестали подключаться после ротации

Слейвы получают ключи действием `sync-keys` перед каждой синхронизацией. Если узел был
недоступен во время ротации, выполните `xraytool syncstates --full` после его возвращения.
Ротация должна быть включена **только на мастере** — иначе узлы будут генерировать разные
ключи.

## Тесты падают локально

* `exec: "xray": executable file not found in $PATH` — установите ядро (`xraytool update-xray`)
  или пропустите пакет `internal/vpn`;
* e2e-набор `tests/` требует свободных портов и рабочего окружения; изолированно
  запускайте нужные пакеты: `go test ./internal/...`.
