# Документация Xraytool: API и Вебхуки

В этом документе описаны все существующие HTTP REST эндпоинты и вебхуки, реализованные на данный момент в бэкенде `xraytool`.

---

## 1. REST API Эндпоинты

Все эндпоинты по пути `/api/` (кроме публичных коллбеков) требуют передачи заголовка `X-API-Key` с мастер-ключом.

### 1.1. Подписки (Для клиентов)
Эти эндпоинты используются VPN-клиентами (V2RayNG, Shadowrocket и т.д.) для обновления профилей.

- `GET /client` - Легаси эндпоинт подписки.
- `GET /api/v1/sub` - Алиас для легаси эндпоинта.
- `GET /api/v2/sub` - Современный JSON эндпоинт. Поддерживает форматы VLESS и возвращает либо валидные профили подключения, либо конфигурации-заглушки.

**Пример запроса:**
```http
GET /api/v2/sub?id=123e4567-e89b-12d3-a456-426614174000
```
**Пример ответа:** (Зависит от клиента, обычно это Base64 строка или JSON массив с узлами)

---

### 1.2. Управление Пользователями
Эндпоинты для регистрации пользователей, работы с балансом и настройками.

#### Регистрация
- `POST /api/v1/users/register`
  Регистрирует нового пользователя и генерирует для него профиль Xray.

**Пример запроса:**
```json
{
  "telegram_id": 123456789,
  "username": "Ivan",
  "telegram_username": "@ivan_tg"
}
```
**Пример ответа:**
```json
{
  "id": "a1b2c3d4-...",
  "username": "Ivan",
  "balance": 0,
  "is_admin": false,
  "max_devices": 3,
  "ref_code": "ref_abc123",
  "referred_by": null,
  "sub_status": "inactive",
  "ends_at": null,
  "starts_at": null,
  "auto_renew": false,
  "referral_count": 0,
  "referral_earned_amount": 0,
  "email": "bot_client_123456789",
  "link": "https://server.com/client?id=a1b2c3d4-...",
  "metadata": {"telegram_id": 123456789, "telegram_username": "@ivan_tg", "source": "telegram_bot"},
  "created_at": "2023-10-01T12:00:00Z"
}
```

#### Получение пользователей
- `GET /api/v1/users`
  Возвращает список всех зарегистрированных пользователей (Массив объектов пользователя, как в примере выше).
- `GET /api/v1/users/admins`
  Возвращает список Telegram ID пользователей с правами администратора.
  **Пример ответа:** `[123456789, 987654321]`
- `GET /api/v1/users/telegram/{id}`
  Получить данные пользователя по его Telegram ID.
- `GET /api/v1/users/ref/{code}`
  Получить данные пользователя по реферальному коду.

#### Управление балансом и устройствами
- `POST /api/v1/users/telegram/{id}/balance`
  Изменить баланс пользователя (добавить или списать средства).
  **Пример запроса:** `{"amount": 100}`
  **Пример ответа:** `{"balance": 100}`

- `POST /api/v1/users/telegram/{id}/max-devices`
  Установить максимальное количество разрешенных устройств для пользователя.
  **Пример запроса:** `{"max_devices": 5}`
  **Пример ответа:** `{"ok": true}`

#### Автопродление и Метаданные
- `POST /api/v1/users/telegram/{id}/auto-renew-toggle`
  Включить/выключить автопродление подписки для пользователя.
  **Пример запроса:** `{"auto_renew": true}`
  **Пример ответа:** `{"ok": true}`

- `POST /api/v1/users/telegram/{id}/auto-renew`
  Запустить процесс автопродления вручную (списывает баланс и продлевает).
  **Пример запроса:**
  ```json
  {
    "plan_total_price": 159,
    "new_ends_at": "2026-07-04T12:00:00Z"
  }
  ```
  **Пример ответа:** `{"ok": true}` (или ошибка 402 при нехватке средств)

- `POST /api/v1/users/telegram/{id}/metadata`
  Установить или обновить произвольные JSON-метаданные пользователя.
  **Пример запроса:** `{"key": "language", "value": "ru"}`
  **Пример ответа:** `{"ok": true}`

#### Устройства пользователя
- `GET /api/v1/users/telegram/{id}/devices`
  Получить массив активных устройств пользователя.
  **Пример ответа:**
  ```json
  [
    {
      "id": "device_uuid",
      "hwid": "abc-123",
      "model": "iPhone 13",
      "os": "iOS",
      "last_active": "2026-06-09T15:00:00Z"
    }
  ]
  ```
- `DELETE /api/v1/users/telegram/{id}/devices/{device_id}`
  Удалить устройство по его ID.
  **Пример ответа:** `{"ok": true}`

---

### 1.3. Платежи
Эндпоинты для работы со счетами, балансом и интеграции с платежными системами.

- `POST /api/v1/payments/create`
  Создать новый платеж/счет.
  **Пример запроса:**
  ```json
  {
    "telegram_id": 123456789,
    "amount": 159,
    "payment_type": "subscription",
    "method": "platega",
    "external_id": "tx_98765"
  }
  ```
  **Пример ответа:** `{"payment_id": 42}`

- `GET /api/v1/payments` (с поддержкой фильтров `?status=...&method=...`)
- `GET /api/v1/payments/{id}`
  Получить информацию о платеже(ах).
  **Пример ответа:**
  ```json
  {
    "id": 42,
    "status": "pending_card",
    "amount": 159,
    "payment_type": "subscription",
    "method": "platega",
    "external_id": "tx_98765",
    "custom_data": {"telegram_id": 123456789},
    "created_at": "2026-06-09T15:00:00Z",
    "user_id": "user_uuid"
  }
  ```

- `POST /api/v1/payments/{id}/status`
  Обновить статус платежа.
  **Пример запроса:**
  ```json
  {
    "status": "completed",
    "expected_statuses": ["pending_card"]
  }
  ```
  **Пример ответа:** `{"ok": true}`

- `POST /api/v1/payments/platega/callback`
  **[ПУБЛИЧНЫЙ]** Вебхук-эндпоинт для Platega.
  **Пример запроса:** (Сырой JSON от Platega, например `{"amount": 159, "status": "success", "tx_id": "tx_98765"}`)
  **Пример ответа:** `{"ok": true}`

---

### 1.4. Действия Администратора
- `POST /api/v1/admin/users/{email}/block`
  Мгновенно блокирует пользователя в Xray.
  **Пример ответа:** `{"ok": true}`
- `POST /api/v1/admin/users/{email}/unblock`
  Разблокирует пользователя.
  **Пример ответа:** `{"ok": true}`
- `POST /api/v1/admin/users/{email}/set-expire`
  Изменяет дату окончания.
  **Пример запроса:** `{"ends_at": "2026-10-01T12:00:00Z"}`
  **Пример ответа:** `{"ok": true}`

---

## 2. Вебхуки (События)

`xraytool` может отправлять `POST` запросы с JSON-данными на любые URL, указанные в файле `config.yaml` в блоке `webhooks`. 
Структура тела запроса (JSON payload) для всех вебхуков выглядит так:
```json
{
  "event_id": "evt_abc123def456",
  "event_type": "название.события",
  "timestamp": "2026-06-09T18:00:00Z",
  "data": { 
    /* специфичные данные события */
  },
  "user_metadata": {
    "telegram_id": 123456789
  }
}
```
*Поле `user_metadata` отправляется только если оно задано у пользователя.*

Ниже описаны структуры объекта `data` для различных событий.

### 2.1. `subscription.expiring`
Срабатывает проактивно когда подписка пользователя скоро закончится.
**Пример `data`:**
```json
{
  "email": "bot_client_123456789",
  "time_left_sec": 86400,
  "warning_level": "24h"
}
```

### 2.2. `subscription.expired`
Срабатывает точно в тот момент, когда время подписки полностью вышло.
**Пример `data`:**
```json
{
  "email": "bot_client_123456789",
  "sub_id": "a1b2c3d4-..."
}
```

### 2.3. `payment.completed`
Срабатывает при переходе статуса платежа в `completed`.
**Пример `data`:**
```json
{
  "payment_id": 42,
  "user_id": "a1b2c3d4-...",
  "amount": 159
}
```

### 2.4. `platega.callback`
Срабатывает, когда шлюз Platega присылает коллбек. Полезно для внешней обработки.
**Пример `data`:** *(Сырой JSON, полученный от Platega)*
```json
{
  "amount": 159,
  "status": "success",
  "tx_id": "tx_98765",
  "custom_field": "..."
}
```

### 2.5. `device.limit_reached`
Срабатывает при попытке подключить устройство поверх лимита.
**Пример `data`:**
```json
{
  "email": "bot_client_123456789",
  "client_id": "123e4567-...",
  "subfile": "123e4567-....txt",
  "hwid": "device_unique_hash",
  "device_limit": 3,
  "device_model": "iPhone 13",
  "device_os": "iOS",
  "ver_os": "17.0",
  "user_agent": "v2rayNG/1.8.5"
}
```
