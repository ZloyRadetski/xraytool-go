# Документация REST API (xraytool-go)

> Оглавление всей документации — [md-docs/README.md](README.md). Смежные разделы:
> [конфигурация](configuration.md), [подписки](subscriptions.md),
> [синхронизация кластера](cluster_sync.md), [вебхуки](webhooks.md).

Ниже представлена **подробнейшая документация по всем API эндпоинтам** с примерами валидных запросов и ответов.

---

## 🔐 Аутентификация

Большинство эндпоинтов (кроме публичных) защищены `authMiddleware`.
Для доступа к ним необходимо передавать HTTP-заголовок `X-API-Key` со значением секретного ключа:
```http
X-API-Key: your_secret_api_key
```

---

## 🌍 Публичные маршруты (Public)

Эти маршруты используются клиентами для получения конфигурации подписки. Авторизация по `X-API-Key` не требуется, но идентификация происходит через query-параметры (например, `?id=...`).

### 1. `GET /client` и `GET /api/v1/sub`
Наследие и первая версия получения конфигурации Xray.

**Пример запроса:**
```http
GET /api/v1/sub?id=123e4567-e89b-12d3-a456-426614174000 HTTP/1.1
Host: api.example.com
```

**Пример ответа:**
Зависит от клиента, обычно это base64 строка или профиль конфигурации для Xray (vless/vmess/trojan ссылки).

### 2. `GET /api/v2/sub`
Новая версия выдачи подписки, которая читает конфигурацию напрямую из SQL базы (GORM).

**Пример запроса:**
```http
GET /api/v2/sub?id=123e4567-e89b-12d3-a456-426614174000 HTTP/1.1
Host: api.example.com
User-Agent: v2rayNG/1.8.5
```

**Пример ответа:**
```http
HTTP/1.1 200 OK
Content-Type: text/plain; charset=utf-8

vless://uuid@server:443?security=tls&encryption=none&type=tcp#bot_client_123
```

---

## 👥 Пользователи (Users)

### 3. `POST /api/v1/users/register`
Регистрация нового пользователя (или получение существующего, идемпотентный метод).

**Пример запроса:**
```http
POST /api/v1/users/register HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "telegram_id": 123456789,
  "username": "Ivanov Ivan",
  "telegram_username": "@ivanov",
  "referred_by_code": "ref_xX11yY22"
}
```

**Пример ответа (201 Created / 200 OK):**
```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "username": "Ivanov Ivan",
  "balance": 0,
  "is_admin": false,
  "max_devices": 3,
  "ref_code": "ref_aB3dE9xZ",
  "referred_by": null,
  "sub_status": "inactive",
  "ends_at": null,
  "starts_at": null,
  "auto_renew": false,
  "referral_count": 0,
  "referral_earned_amount": 0,
  "email": "bot_client_123456789",
  "link": "https://domain.com/client?id=xray-uuid-here",
  "metadata": {
    "source": "telegram_bot",
    "telegram_id": 123456789,
    "telegram_username": "@ivanov"
  },
  "created_at": "2026-06-11T12:00:00Z"
}
```


### 3.1. `POST /api/v1/users/request_code`
Запрос кода для авторизации на сайте через Telegram. При вызове бэкенд xraytool **сам генерирует 6-значный код** и сохраняет его во временном кэше (на 5 минут), а затем генерирует webhook `auth.request_code`, который ловит Python-бот и отправляет этот код пользователю в Telegram.

**Пример запроса:**
```http
POST /api/v1/users/request_code HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "telegram_id": 123456789
}
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

### 3.2. `POST /api/v1/users/verify_code`
Проверка введенного кода подтверждения. Если код верный, бэкенд возвращает 200 OK, и SvelteKit может авторизовать пользователя.

**Пример запроса:**
```http
POST /api/v1/users/verify_code HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "telegram_id": 123456789,
  "code": "843912"
}
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```
*(В случае неверного или просроченного кода вернется 401 Unauthorized)*

### 3.3. `POST /api/v1/users/link_session`
Привязка временной сессии Telegram Deep Link к Telegram ID пользователя. Бэкенд генерирует 6-значный код и сохраняет его во временном кэше (на 5 минут), привязывая `telegram_id` в качестве payload к `session_id`.

**Пример запроса:**
```http
POST /api/v1/users/link_session HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "session_id": "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx",
  "telegram_id": 123456789
}
```

**Пример ответа (200 OK):**
```json
{
  "ok": true,
  "code": "843912"
}
```
*(В случае превышения лимитов запросов вернется 429 Too Many Requests. Если session_id имеет неверный формат UUID, вернется 400 Bad Request.)*

### 3.4. `POST /api/v1/users/verify_session`
Проверка введенного кода подтверждения для сессии Telegram Deep Link. Если код верный, возвращается `telegram_id` пользователя и признак `is_admin` для авторизации на сайте.

**Пример запроса:**
```http
POST /api/v1/users/verify_session HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "session_id": "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx",
  "code": "843912"
}
```

**Пример ответа (200 OK):**
```json
{
  "ok": true,
  "telegram_id": 123456789,
  "is_admin": false
}
```
*(В случае неверного или просроченного кода вернется 401 Unauthorized. После 3 неудачных попыток ввода кода вернется 403 Forbidden.)*

### 4. `GET /api/v1/users`
Получение списка всех пользователей.

**Пример запроса:**
```http
GET /api/v1/users HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):**
```json
[
  {
    "id": "...",
    "username": "Ivanov Ivan",
    "balance": 150,
    ...
  }
]
```

### 5. `GET /api/v1/users/admins`
Получение списка Telegram ID администраторов.

**Пример запроса:**
```http
GET /api/v1/users/admins HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):**
```json
[
  123456789,
  987654321
]
```

### 6. `GET /api/v1/users/{platform}/{id}`
Получение пользователя по его платформенному ID.

`platform` может быть:
- `telegram` (поиск по `telegram_id` в метаданных)
- `web` (поиск по `email` в метаданных)
- `uuid` (поиск по основному UUID)

**Пример запроса:**
```http
GET /api/v1/users/telegram/123456789 HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):** *(См. структуру ответа регистрации)*

### 7. `GET /api/v1/users/{platform}/{id}/devices`
Получение списка подключенных устройств пользователя.

**Пример запроса:**
```http
GET /api/v1/users/telegram/123456789/devices HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):**
```json
[
  {
    "id": "dev_123",
    "subscription_id": "sub_123",
    "name": "iPhone 13",
    "last_ip": "192.168.1.1",
    "last_seen_at": "2026-06-10T15:30:00Z",
    "created_at": "2026-06-01T10:00:00Z"
  }
]
```

### 8. `DELETE /api/v1/users/{platform}/{id}/devices/{device_id}`
Удаление устройства пользователя (с авто-разблокировкой при необходимости).

**Пример запроса:**
```http
DELETE /api/v1/users/telegram/123456789/devices/dev_123 HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

### 9. `GET /api/v1/users/ref/{code}`
Получение пользователя по реферальному коду.

**Пример запроса:**
```http
GET /api/v1/users/ref/ref_aB3dE9xZ HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):** *(См. структуру ответа регистрации)*

### 10. `POST /api/v1/users/{platform}/{id}/balance`
Атомарное изменение баланса пользователя. Для списания передайте отрицательное значение.

**Пример запроса:**
```http
POST /api/v1/users/telegram/123456789/balance HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "amount": 100
}
```

**Пример ответа (200 OK):**
```json
{
  "balance": 250
}
```

### 11. `POST /api/v1/users/{platform}/{id}/max-devices`
Установка максимального количества разрешенных устройств.

**Пример запроса:**
```http
POST /api/v1/users/telegram/123456789/max-devices HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "max_devices": 5
}
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

### 12. `POST /api/v1/users/{platform}/{id}/auto-renew-toggle`
Включение/выключение автопродления подписки.

**Пример запроса:**
```http
POST /api/v1/users/telegram/123456789/auto-renew-toggle HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "auto_renew": true
}
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

### 13. `POST /api/v1/users/{platform}/{id}/auto-renew`
Атомарное продление подписки со списанием средств с баланса и автоматическим разбаном в Xray.

**Пример запроса:**
```http
POST /api/v1/users/telegram/123456789/auto-renew HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "plan_total_price": 159,
  "new_ends_at": "2026-07-11T12:00:00Z"
}
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```
*(При нехватке средств вернется `402 Payment Required`)*

### 14. `POST /api/v1/users/{platform}/{id}/metadata`
Обновление/добавление кастомного ключа в `metadata` пользователя.

**Пример запроса:**
```http
POST /api/v1/users/telegram/123456789/metadata HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "key": "has_trial",
  "value": false
}
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

---

## 💳 Платежи (Payments)

### 15. `POST /api/v1/payments/create`
Создание записи о новом платеже.

**Пример запроса:**
```http
POST /api/v1/payments/create HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "telegram_id": 123456789,
  "amount": 159,
  "payment_type": "subscription",
  "method": "platega",
  "external_id": "ext_invoice_998877",
  "plan_id": 1,
  "promo_code": "SUMMER20",
  "platform": "bot"
}
```
*(Если `external_id` пустой, будет сгенерирован автоматически. Если передан `plan_id`, поле `amount` будет проигнорировано и итоговая сумма будет рассчитана на сервере с учетом промокода и глобальной скидки. `promo_code` и `platform` опциональны).*

**Пример ответа (201 Created):**
```json
{
  "payment_id": 42
}
```

### 16. `GET /api/v1/payments`
Получение списка платежей с возможностью фильтрации.
*Поддерживаемые фильтры:* `status`, `method`, `payment_type`, `telegram_id`.

**Пример запроса:**
```http
GET /api/v1/payments?status=completed&method=platega HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):**
```json
[
  {
    "id": 42,
    "status": "completed",
    "amount": 159,
    "payment_type": "subscription",
    "method": "platega",
    "external_id": "ext_invoice_998877",
    "custom_data": {
      "telegram_id": 123456789
    },
    "created_at": "2026-06-11T12:00:00Z",
    "user_id": "a1b2c3d4..."
  }
]
```

### 17. `GET /api/v1/payments/{id}`
Получение информации о конкретном платеже.

**Пример запроса:**
```http
GET /api/v1/payments/42 HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):** *(См. структуру одного платежа выше)*

### 18. `POST /api/v1/payments/{id}/status`
Атомарное обновление статуса платежа. Если статус становится `completed`, будет автоматически начислено реферальное вознаграждение (25%).

**Пример запроса:**
```http
POST /api/v1/payments/42/status HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "status": "completed",
  "expected_statuses": ["pending_card", "created"]
}
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

---

## 🪝 Вебхуки (Webhooks)

### 19. `POST /api/v1/payments/platega/callback`
Прием вебхука от платежного шлюза Platega. Защищен проверкой HMAC SHA-256 подписи в заголовке `X-Platega-Signature`.

**Пример входящего запроса (от Platega):**
```http
POST /api/v1/payments/platega/callback HTTP/1.1
Content-Type: application/json
X-Platega-Signature: 6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b

{
  "external_id": "ext_invoice_998877",
  "status": "completed",
  "amount": 159,
  "currency": "RUB",
  "test_mode": false
}
```

**Пример ответа сервера (200 OK):**
```json
{
  "ok": true
}
```

---

## 🛡 Администрирование (Admin)

Действия напрямую влияют на конфигурацию ядра Xray.

### 20. `POST /api/v1/admin/users/{email}/block`
Блокировка пользователя (моментальное удаление из Xray и добавление в limitedDB).

**Пример запроса:**
```http
POST /api/v1/admin/users/bot_client_123456789/block HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

### 21. `POST /api/v1/admin/users/{email}/unblock`
Разблокировка пользователя (восстановление в Xray). Опционально можно передать новый лимит устройств.

**Пример запроса:**
```http
POST /api/v1/admin/users/bot_client_123456789/unblock HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "limit": 5
}
```
*(Body опционально)*

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

### 22. `POST /api/v1/admin/users/{email}/set-expire`
Изменение даты окончания подписки пользователя (с авто-апдейтом в ядре Xray).

**Пример запроса:**
```http
POST /api/v1/admin/users/bot_client_123456789/set-expire HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "expire": "2026-12-31T23:59:59Z"
}
```
*(Поддерживаются форматы RFC3339, `2006-01-02T15:04:05Z`, просто `YYYY-MM-DD`, а также удобные форматы по МСК: `ДД.ММ.ГГГГ ЧЧ:ММ` и `ДД.ММ.ГГГГ`)*

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

### 23. `POST /api/v1/admin/users/{platform}/{id}/global-ban`
Глобальная блокировка пользователя. Отключает подписку, удаляет из Xray и устанавливает флаг `is_blocked = true`. Пользователь не сможет продлевать подписку или покупать новую, пока не будет разбанен.

**Пример запроса:**
```http
POST /api/v1/admin/users/telegram/123456789/global-ban HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

### 24. `POST /api/v1/admin/users/{platform}/{id}/global-unban`
Снятие глобальной блокировки пользователя. Устанавливает флаг `is_blocked = false` и, если подписка активна, немедленно возвращает доступ в Xray.

**Пример запроса:**
```http
POST /api/v1/admin/users/telegram/123456789/global-unban HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

### 24.1. `DELETE /api/v1/admin/users/{platform}/{id}`
Полное удаление пользователя из базы данных и выдворение из Xray. Действие необратимо.

**Пример запроса:**
```http
DELETE /api/v1/admin/users/telegram/123456789 HTTP/1.1
X-API-Key: secret
```

**Пример ответа (200 OK):**
```json
{
  "ok": true
}
```

---
## 💳 Тарифы и Промокоды (Protected)

### `GET /api/v1/plans`
Получение списка активных тарифных планов. Бэкенд автоматически применяет глобальные скидки.
* **Ответ (200 OK):**
```json
[
  {
    "id": 1,
    "months": 1,
    "base_price": 159,
    "discount_percent": 10,
    "final_price": 143
  }
]
```

### `GET /api/v1/promocodes/validate`
Проверка актуальности промокода для конкретной платформы.
* **Query параметры:**
  * `code` (строка, обязательно) - Сам промокод (например SUMMER20).
  * `platform` (строка, обязательно) - Платформа (bot или web).
* **Ответ (200 OK):**
```json
{
  "valid": true,
  "discount_percent": 20,
  "id": 1
}
```
* **Ответ при ошибке (400 Bad Request):**
```json
{
  "error": "promo code usage limit reached"
}
```

### 25. `GET /api/v1/admin/users`
Получение полного списка пользователей для админ-панели.

### 26. `GET /api/v1/admin/payments/stats`
Получение статистики по платежам (сумма, количество и т.д.).

### 27. `GET /api/v1/admin/antifraud/state`
Просмотр текущего состояния системы антифрода (метрики аномалий).

### 28. `POST /api/v1/admin/promocodes`
Создание нового промокода.
**Пример запроса:**
```http
POST /api/v1/admin/promocodes HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "code": "NEW20",
  "discount_percent": 20,
  "max_uses": 100,
  "target_platform": "all",
  "is_active": true
}
```

### 29. `GET /api/v1/admin/promocodes`
Список всех промокодов.

### 30. `PUT /api/v1/admin/promocodes/{id}`
Редактирование промокода.

### 31. `DELETE /api/v1/admin/promocodes/{id}`
Удаление промокода.

---
## ⚙️ Внутренние методы (Internal)

### 32. `POST /api/v1/internal/xray/sync`
Единый роут для кластерной синхронизации и общения Master-Slave. Принимает тело JSON, где поле `action` определяет операцию.

**Пример синхронизации пакета изменений (apply-batch):**
```http
POST /api/v1/internal/xray/sync HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "action": "apply-batch",
  "payload": "{\"add\": [{\"Email\": \"bot_client_1\"}], \"remove\": [\"old_uuid\"]}"
}
```

**Пример создания пользователя на слейве (newuser):**
```http
POST /api/v1/internal/xray/sync HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "action": "newuser",
  "email": "bot_client_123",
  "uuid": "xray-uuid-string",
  "expire": "2026-12-31T23:59:59Z",
  "limit": "3",
  "subfile": "xray-uuid-string",
  "auth": "secret-auth-token"
}
```
*(Поддерживаемые actions: `apply-batch`, `newuser`, `rmuser`, `setexpire`, `limit`, `setlimit`, `unlimit`, `usersnapshot`, `cli-stats`, `antifraud-events`)*

---
## 📁 REST Файлы и Конфиги

Эндпоинты для обновления статических ссылок и загрузки файлов.

### 33. `POST /api/rest/update-links`
Обновление публичных ссылок для скачивания клиентов. Требует авторизации.

### 34. `POST /api/rest/upload`
Загрузка файлов на бэкенд (сертификаты, конфигурации).

### 35. `GET /api/rest/download`
Скачивание файлов с бэкенда.

---
## 📤 Исходящие вебхуки (Outbound Webhooks)

`xraytool-go` умеет отправлять асинхронные HTTP POST-запросы на адреса, указанные в `config.yaml` (`webhooks`). Вебхуки полезны для мгновенного уведомления Telegram-бота о событиях на бэкенде.

**Формат исходящего вебхука:**
```json
{
  "event_id": "evt_abc123",
  "event_type": "event.name",
  "timestamp": "2026-06-11T12:00:00Z",
  "data": {
    "key": "value"
  },
  "user_metadata": {
    "telegram_id": 123456
  }
}
```

**Доступные типы событий (`event_type`):**

#### 1. `auth.request_code`
Генерируется при вызове `/api/v1/users/request_code`. Бот ловит это событие и отправляет код подтверждения пользователю.
* **Data:** `{"telegram_id": 12345, "code": "843912"}`

#### 2. `payment.completed`
Генерируется после успешного завершения платежа.
* **Data:** `{"payment_id": 42, "amount": 159, "telegram_id": 12345}`

#### 3. `referral.reward`
Генерируется, когда пользователю начислен реферальный процент.
* **Data:** `{"user_id": "uuid...", "amount": 39, "from_payment_id": 42, "telegram_id": 12345}`

#### 4. `platega.callback`
Трансляция сырого вебхука от платежки Platega.
* **Data:** `{"external_id": "...", "status": "completed", ...}`
