# Синхронизация кластера (Master → Slave)

Пакеты: `internal/statesync`, `internal/slave`, обработчики `internal/server/handlers_sync.go`
и `internal/server/handlers_internal.go`.

## 1. Идея

Мастер — единственный источник правды. Каждое изменение VPN-пользователя записывается в
append-only журнал `sync_events` **в той же транзакции**, что и изменение подписки. Позиция
кластера описывается двумя значениями (таблица `sync_state`, всегда одна строка `id=1`):

* `last_event_id` — максимальный применённый идентификатор события;
* `state_hash` — накопительный SHA-256: `new = sha256(old + event_id + action + payload)`.

Слейв ведёт такую же пару значений у себя. Сравнение пары «id + хеш» позволяет за один
дешёвый запрос понять, расходятся ли узлы.

На мастере движок обёрнут в `statesync.EventAwareEngine`: любой вызов `AddUser` /
`RemoveUser` автоматически добавляет событие `add` / `update` / `remove` в журнал.

События старше **7 дней** удаляются (`PurgeOldEvents`).

## 2. Трёхфазный протокол

`slave.stateSyncProvider.SyncAllSlaves` обходит все узлы из `slave_servers` параллельно:

```
0. sync-keys        (если reality.rotation_enabled) — разослать актуальные Reality-ключи
1. sync-ping        — «мой last_event_id=N, hash=H; у тебя так же?»
   ├── match=true   → узел актуален, работа закончена
   └── match=false  → шаг 2
2. sync-delta       — отправить упорядоченный список событий после last_event_id слейва
   └── недоступно (событий 0, > 500 или они уже вычищены) → шаг 3
3. sync-full-trigger— слейв в фоне забирает полный снапшот постранично и вызывает SyncUsers
```

Все три действия отправляются одним и тем же POST-запросом на sync-эндпоинт слейва
(`slave_api.remote_path`, по умолчанию `/api/v1/internal/xray/sync`) с заголовком `X-API-Key`
и телом:

```json
{"action": "sync-ping", "payload": "<last_event_id>", "auth": "<state_hash>", "uuid": "<target_event_id>"}
```

Поля `payload`, `auth`, `uuid` переиспользуются под разные значения в зависимости от действия
(это осознанное упрощение существующего протокола).

### Ответы

| Действие | Ответ слейва |
| --- | --- |
| `sync-ping` | `{"match": true}` или `{"match": false, "last_event_id": 42}` |
| `sync-delta` | `{"ok": true, "applied": 17}` |
| `sync-full-trigger` | `{"ok": true, "status": "full_sync_started"}` (обработка асинхронная) |
| `sync-keys` | `{"ok": true}` |

### Delta

Слейв применяет события **строго последовательно**; при первой ошибке возвращается `500`, и
мастер повторит попытку на следующем цикле. Каждое применённое событие дописывается в
локальный журнал слейва, затем `sync_state` устанавливается в целевую позицию мастера.

Порог перехода на полную синхронизацию — `maxDeltaEvents = 500`.

### Full-sync

Слейв немедленно отвечает `ok` и в фоне (таймаут 30 минут) постранично тянет с мастера:

```
GET /api/v1/internal/xray/sync/snapshot?offset=0&limit=1000
X-API-Key: <ключ>
→ {"users": [...], "has_more": true, "total": 12345}
```

Размер страницы — 1000 записей, ответ на каждый чанк ограничен 10 МБ, поэтому потребление
памяти не зависит от размера базы. Затем вызывается `Engine.SyncUsers(users, prune=true)`:
лишние клиенты удаляются, недостающие добавляются. В конце сохраняется целевая пара
`last_event_id` + `state_hash`, и следующий ping вернёт `match=true`.

Базовый URL слейв берёт из `master_api.url`, автоматически дополняя путь до `/snapshot`
(поддерживаются как «legacy»-конфиги с полным путём до `/sync`, так и корневые URL).

## 3. Что попадает в снапшот

`statesync.Service.BuildSnapshot` включает подписку, если:

* заполнены `email` и `xray_uuid`;
* `status == "active"`;
* пользователь не заблокирован администратором (`users.is_blocked`);
* нет активного антифрод-бана (`antifraud_bans`).

Тот же фильтр применяется в `SelfHealMasterUUIDs` — периодической сверке рантайма мастера
с базой.

## 4. Эндпоинты мастера

| Метод | Путь | Назначение |
| --- | --- | --- |
| `GET` | `/api/v1/internal/xray/sync/state` | Текущее состояние мастера (`last_event_id`, `state_hash`) |
| `GET` | `/api/v1/internal/xray/sync/snapshot?offset=&limit=` | Постраничный снапшот активных пользователей |
| `POST` | `/api/v1/internal/xray/sync` | Универсальный sync-эндпоинт (см. ниже) |

## 5. Действия универсального sync-эндпоинта

Помимо трёх фаз синхронизации, `POST /api/v1/internal/xray/sync` принимает точечные команды —
их использует мастер для мгновенного распространения изменений (`EventPropagator`):

| `action` | Обязательные поля | Что делает на слейве |
| --- | --- | --- |
| `newuser` | `email`, `uuid` | Создаёт клиента (`SkipDB=true` — только движок) |
| `rmuser` | `email` | Удаляет клиента |
| `limit` | `email` | Блокирует клиента |
| `unlimit` | `email`, `limit` | Возвращает клиента |
| `setlimit` | `email`, `limit` | Меняет лимит устройств |
| `setexpire` | `email`, `expire` | Меняет срок действия |
| `sync-users` | `payload` (массив `VPNUserConfig`) | Полная сверка списка пользователей |
| `sync-keys` | `payload` (JSON ключей) | Сохраняет Reality-ключи и перестраивает конфиг |
| `cli-stats` | — | Возвращает локальную статистику трафика |
| `antifraud-events` | `payload` (`{"events":[{"Email","IP"}]}`) | Приём IP-событий слейва мастером |
| `sync-ping` / `sync-delta` / `sync-full-trigger` | см. выше | Фазы синхронизации |

## 6. Ротация Reality-ключей

1. `xraytool rotate-keys` (или включённая автоматическая ротация) генерирует X25519-пару и
   15 Short ID, сохраняя их в `reality.keys_filepath` (`0600`).
2. Локальный `config.json` перестраивается из шаблона с новыми ключами.
3. Перед каждым циклом синхронизации мастер рассылает файл ключей действием `sync-keys`.
4. Слейв записывает ключи и вызывает `SyncRealityKeys`, перестраивая свой конфиг «на лету».
5. В подписке клиенту отдаётся публичный ключ (`{PBK}`) и Short ID (`{SID}`), выбираемый
   детерминированно: `sha256(subscription_id) mod len(short_ids)` — у пользователя всегда
   один и тот же Short ID до следующей ротации.

## 7. Запуск синхронизации

* Фоновый воркер `SyncStatesWorker` — каждые `worker.sync_states_interval` (по умолчанию 3м).
* Хук `OnConfigModified` адаптера — сразу после изменения конфига на мастере.
* Вручную: `xraytool syncstates [--full] [--dry-run]`.

Параллельные запуски защищены `TryLock`: если синхронизация уже идёт, новый вызов тихо
пропускается.

## 8. Диагностика

```bash
# Состояние мастера
curl -s -H "X-API-Key: $KEY" https://master/api/v1/internal/xray/sync/state

# Первая страница снапшота
curl -s -H "X-API-Key: $KEY" "https://master/api/v1/internal/xray/sync/snapshot?offset=0&limit=5"

# Принудительная полная синхронизация всех слейвов
xraytool syncstates --full
```

Типовые причины `match=false` навсегда: разные `api_key`, рассинхронизация системного времени,
ручная правка `config.json` на слейве (её затрёт ближайший full-sync).
