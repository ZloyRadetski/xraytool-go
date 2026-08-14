# Документация REST API (xraytool-go)
Ниже приведена документация актуальных HTTP-маршрутов, которые регистрирует текущая версия приложения и включённые плагины. Наличие маршрута плагина зависит от того, включён ли этот плагин в конфигурации.

---

## 🔐 Аутентификация

Маршруты core, billing, promo, config_storage, support_chat и внутренней синхронизации (кроме callback платёжного провайдера) защищены `authMiddleware`.
Для доступа к ним необходимо передавать HTTP-заголовок `X-API-Key` со значением секретного ключа:
```http
X-API-Key: your_secret_api_key
```

Публичные маршруты подписки не требуют этого заголовка. Callback платёжного провайдера проверяется самим провайдером. Маршруты `support_chat` дополнительно получают идентификатор пользователя от доверенного приложения; не передавайте `X-API-Key` в браузер и не открывайте этот API в обход серверного proxy.

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

**Параметр `format`:**

| Значение | Content-Type | Описание |
|---|---|---|
| (не указан) | `application/json` | Xray JSON конфигурация |
| `vless` | `text/plain` | Share-ссылки (vless/vmess/trojan/ss/hy2) |
| `clash` | `text/yaml` | Подписка в формате Clash/Mihomo (`proxies`, `proxy-groups`, `rules`) |

```http
GET /api/v2/sub?id=123e4567-e89b-12d3-a456-426614174000&format=clash HTTP/1.1
Host: api.example.com
```

```yaml
proxies:
    - name: VLESS-Reality
      type: vless
      server: 1.2.3.4
      port: 443
      uuid: 123e4567-e89b-12d3-a456-426614174000
      flow: xtls-rprx-vision
      tls: true
      servername: www.google.com
      reality-opts:
        public-key: PBK
        short-id: ab12
proxy-groups:
    - name: PROXY
      type: select
      proxies: [AUTO, VLESS-Reality]
rules:
    - MATCH,PROXY
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

### 3.5. `POST /api/v1/users/link/telegram`

Привязывает web-аккаунт к Telegram-аккаунту после подтверждения кода, выданного для сессии.

```http
POST /api/v1/users/link/telegram HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "session_id": "session-uuid",
  "code": "123456",
  "email": "user@example.com"
}
```

Ответ: `200 OK` и `{ "ok": true }`. Возможные ошибки: `400` для неполного запроса, `401` для неверного/истёкшего кода, `403` при превышении числа попыток и `404`, если один из аккаунтов не найден.

### 3.6. `POST /api/v1/users/link/email`

Подтверждает адрес электронной почты и привязывает его к Telegram-аккаунту.

```http
POST /api/v1/users/link/email HTTP/1.1
Content-Type: application/json
X-API-Key: secret

{
  "telegram_id": 123456789,
  "email": "user@example.com",
  "code": "123456"
}
```

Ответ: `200 OK` и `{ "ok": true }`. Поле `email` нормализуется и валидируется сервером.

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

### 19. `POST /api/v1/payments/{method}/callback`
Прием вебхука от подключенного платежного провайдера. Маршрут не требует
`X-API-Key`: провайдер сам проверяет подпись или секрет и возвращает
нормализованный статус платежа. Неизвестный или отключенный `method` вернет
`404`.

Для встроенного Platega `method` равен `platega`, а обязательный заголовок —
`X-Secret` со значением `plugins.payment_platega.config.secret`.

**Пример входящего запроса (от Platega):**
```http
POST /api/v1/payments/platega/callback HTTP/1.1
Content-Type: application/json
X-Secret: configured-platega-secret

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
Постраничный список пользователей для админ-панели.

Необязательные query-параметры: `page` (по умолчанию `1`), `limit` (по умолчанию `50`, максимум `100`) и `search`.

```json
{
  "total": 125,
  "page": 1,
  "limit": 50,
  "users": [{ "id": "user-uuid", "balance": 100 }]
}
```

### 26. `GET /api/v1/admin/payments/stats`
Получение помесячной статистики по всем платежам. Массив сортируется от нового месяца к старому.

```json
[
  {
    "month": "2026-08",
    "total_revenue": 15900,
    "completed_count": 100,
    "total_count": 112
  }
]
```

### 27. `GET /api/v1/admin/antifraud/state`
Просмотр текущего состояния системы антифрода. Если плагин или его snapshot provider отключён, ответ будет `{ "enabled": false }`; при включённом провайдере сервер добавляет `{ "enabled": true }` к данным его snapshot. Ошибка недоступного провайдера — `502 Bad Gateway`.

Поле `hash_key_id` в ответе — безопасный идентификатор HMAC-ключа антифрода. На master и reporting-slave оно должно совпадать; сам секрет и IP-адреса в это поле не входят.

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

### 32. Cluster replication (gRPC only)

The historic HTTP synchronisation endpoint `POST /api/v1/internal/xray/sync` and its `/state` and `/snapshot` variants were removed. Cluster traffic now uses the `cluster_replication` plugin: a TLS 1.3 mTLS gRPC stream with durable outbox/inbox positions, framed user snapshots, configuration artifacts and automatic slave drift repair. It is not part of the public REST API. See [cluster replication](cluster_replication.md) for configuration and migration.

The material below is retained only as an archival description of the removed endpoint; do not implement or call it.
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

### 32.1. Removed historical `GET /api/v1/internal/xray/sync/state`

Возвращает состояние master-узла для быстрой проверки расхождения со slave. Доступен только если подключён cluster-sync provider.

```http
GET /api/v1/internal/xray/sync/state HTTP/1.1
X-API-Key: secret
```

Ответ `200 OK` содержит актуальные `last_event_id` и `state_hash`. Если provider не подключён, возвращается `503`.

### 32.2. Removed historical `GET /api/v1/internal/xray/sync/snapshot`

Возвращает страницу полного снимка VPN-пользователей для slave-узла.

```http
GET /api/v1/internal/xray/sync/snapshot?offset=0&limit=1000 HTTP/1.1
X-API-Key: secret
```

Параметр `offset` не может быть отрицательным. `limit` по умолчанию и максимум — `1000`.

```json
{
  "users": [{ "email": "user@example.com" }],
  "has_more": false,
  "total": 1
}
```

---
## 📁 REST Файлы и Конфиги

Эндпоинты для обновления статических ссылок и загрузки файлов.

### 33. `POST /api/rest/update-links`
Обновление файла и отправка события `file.uploaded`. Запрос — `multipart/form-data` с полями `path` и `file`; максимальный размер — 10 MiB. `path` должен находиться внутри `server.allowed_dirs`.

Ответ: `200 OK`, `{ "status": "success", "message": "file saved & webhook dispatched" }`.

### 34. `POST /api/rest/upload`
Загрузка служебного файла через `multipart/form-data` с полями `path` и `file`. Максимальный размер — 10 MiB; путь проверяется по `server.allowed_dirs`; файлы с расширением `.exe`, `.yaml` и `.yml` отклоняются.

Ответ: `200 OK`, `{ "status": "success", "message": "file saved" }`.

### 35. `GET /api/rest/download`
Скачивание ранее разрешённого файла. Обязателен query-параметр `path`; он также проверяется по `server.allowed_dirs`. В ответе передаётся исходный бинарный поток файла.

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

#### 4. `{method}.callback`
Трансляция проверенного вебхука платежного провайдера. Для Platega имя
события — `platega.callback`.
* **Data:** `{"external_id": "...", "status": "completed", ...}`

---

## 💬 Техподдержка (Support Chat Plugin)

Плагин `support_chat` предоставляет чат пользователя с администраторами. Это не E2EE: сервер расшифровывает данные, чтобы отдать их авторизованному участнику. При этом текст сообщений, темы тикетов, пользовательские идентификаторы и метаданные вложений шифруются на хранении; новые файлы используют аутентифицированное потоковое шифрование с отдельным ключом на вложение.

### Клиентские методы (Client API)

Все маршруты плагина оборачиваются основным `authMiddleware`. Идентификатор клиента берётся из server context, затем из заголовка `X-User-ID` или `X-Telegram-ID`; query-параметры для идентификации не используются.

> Важно: `X-API-Key` аутентифицирует только доверенное приложение, а не конечного пользователя. Оно обязано устанавливать идентификатор пользователя на сервере. Административные маршруты и доступ администратора к вложениям дополнительно сверяют флаг `is_admin` в хранилище пользователей xraytool; параметр `admin=true` не предоставляет никаких прав.

#### 1. Создать тикет
`POST /api/v1/support/conversations`

**Тело запроса (JSON):**
```json
{
  "subject": "Проблема с оплатой",
  "message": "Здравствуйте, оплатил подписку по СБП, но она не пришла.",
  "attachments": ["uuid-вложения-1", "uuid-вложения-2"]
}
```
*(Поле `attachments` опционально, должно содержать массив ID предварительно загруженных файлов).*

**Ответ (200 OK):**
```json
{
  "conversation_id": "conv-12345-uuid",
  "created_at": "2026-08-07T21:46:56Z",
  "message": {
    "id": "msg-9876-uuid",
    "conversation_id": "conv-12345-uuid",
    "sender_role": "client",
    "text": "Здравствуйте, оплатил подписку по СБП, но она не пришла.",
    "created_at": "2026-08-07T21:46:56Z",
    "read_at": null
  }
}
```

#### 2. Получить список своих тикетов
`GET /api/v1/support/conversations`

**Ответ (200 OK):**
```json
{
  "conversations": [
    {
      "id": "conv-12345-uuid",
      "user_id": "user-telegram-id",
      "subject": "Проблема с оплатой",
      "status": "open",
      "created_at": "2026-08-07T21:46:56Z",
      "updated_at": "2026-08-07T21:46:56Z"
    }
  ]
}
```

#### 3. Получить информацию о конкретном тикете
`GET /api/v1/support/conversations/{id}`

**Ответ (200 OK):**
```json
{
  "id": "conv-12345-uuid",
  "user_id": "user-telegram-id",
  "subject": "Проблема с оплатой",
  "status": "open",
  "created_at": "2026-08-07T21:46:56Z",
  "updated_at": "2026-08-07T21:46:56Z"
}
```

#### 4. Удалить тикет (чат)
`DELETE /api/v1/support/conversations/{id}`

Удаляет тикет, всю историю сообщений и прикрепленные файлы из базы данных и файлового хранилища (если файлы не используются другими тикетами). Пользователь может удалять только свои собственные тикеты.

**Ответ (200 OK):**
```json
{
  "status": "ok"
}
```

#### 5. Получить историю сообщений (и пометить прочитанными)
`GET /api/v1/support/conversations/{id}/messages`

При вызове этого эндпоинта все сообщения от администратора в этом тикете автоматически помечаются как прочитанные клиентом.

**Ответ (200 OK):**
```json
{
  "messages": [
    {
      "id": "msg-9876-uuid",
      "conversation_id": "conv-12345-uuid",
      "sender_role": "client",
      "text": "Здравствуйте, оплатил подписку по СБП, но она не пришла.",
      "created_at": "2026-08-07T21:46:56Z",
      "read_at": "2026-08-07T21:50:00Z"
    },
    {
      "id": "msg-9999-uuid",
      "conversation_id": "conv-12345-uuid",
      "sender_role": "admin",
      "text": "Подписка выдана, извините за задержку.",
      "created_at": "2026-08-07T21:48:00Z",
      "read_at": "2026-08-07T21:51:00Z"
    }
  ]
}
```

#### 5. Отправить сообщение в тикет
`POST /api/v1/support/conversations/{id}/messages`

**Тело запроса (JSON):**
```json
{
  "text": "Спасибо, всё заработало!",
  "attachments": ["uuid-вложения-1"]
}
```
*(Поле `attachments` опционально).*

**Ответ (200 OK):**
```json
{
  "id": "msg-1010-uuid",
  "conversation_id": "conv-12345-uuid",
  "sender_role": "client",
  "text": "Спасибо, всё заработало!",
  "created_at": "2026-08-07T21:55:00Z",
  "read_at": null
}
```

#### 6. Загрузить вложение
`POST /api/v1/support/attachments`

Загружает файл на сервер. Обязательно передавать файл через `multipart/form-data` в поле `file`.

**Пример запроса (curl):**
```bash
curl -X POST /api/v1/support/attachments \
  -H "X-User-ID: user-uuid" \
  -F "file=@/path/to/image.png"
```

**Ответ (200 OK):**
```json
{
  "id": "att-12345-uuid",
  "file_name": "image.png",
  "mime_type": "image/png"
}
```

#### 7. Скачать вложение
`GET /api/v1/support/attachments/{id}/download`

Возвращает расшифрованный файл с `Cache-Control: no-store`. Доступ имеет только отправитель файла (если файл ещё не привязан к сообщению), владелец тикета или пользователь с серверным флагом `is_admin`. Параметр `?admin=true` игнорируется и не может использоваться для получения доступа.

**Пример ответа (200 OK):**
(Бинарный поток файла с соответствующим `Content-Type`)

#### 8. Клиентский WebSocket (Real-time)
`GET /api/v1/support/conversations/{id}/ws`
Требуется передавать авторизационный токен.

**Формат входящих сообщений по WS:**
Событие нового сообщения от админа:
```json
{
  "type": "new_message",
  "payload": {
    "id": "msg-9999-uuid",
    "conversation_id": "conv-12345-uuid",
    "sender_role": "admin",
    "text": "Подписка выдана, извините за задержку.",
    "created_at": "2026-08-07T21:48:00Z",
    "read_at": null
  }
}
```

Событие изменения статуса тикета (например, админ закрыл):
```json
{
  "type": "status_changed",
  "payload": {
    "conversation_id": "conv-12345-uuid",
    "status": "closed"
  }
}
```

#### 9. Проверить статус блокировки в саппорте
`GET /api/v1/support/ban-status`

Позволяет клиенту проверить, ограничен ли доступ к саппорту.

**Пример ответа (не заблокирован):**
```json
{
  "is_banned": false
}
```

**Пример ответа (заблокирован):**
```json
{
  "is_banned": true,
  "reason": "Спам и ненормативная лексика",
  "expires_at": "2026-08-20T12:00:00Z"
}
```

---

### Администраторские методы (Admin API)

Админские методы требуют повышенных привилегий.

#### 1. Получить список всех тикетов (с фильтрацией)
`GET /api/v1/admin/support/conversations`

**Query-параметры (опционально):**
- `?status=open` (или `closed`, `resolved`)
- `?user_id=123456789`

**Ответ (200 OK):**
```json
{
  "conversations": [
    {
      "id": "conv-12345-uuid",
      "user_id": "user-telegram-id",
      "subject": "Проблема с оплатой",
      "status": "open",
      "created_at": "2026-08-07T21:46:56Z",
      "updated_at": "2026-08-07T21:46:56Z"
    }
  ]
}
```

#### 2. Удалить тикет (чат) администратором
`DELETE /api/v1/admin/support/conversations/{id}`

Удаляет любой тикет, историю сообщений и прикрепленные файлы из базы данных и файлового хранилища (если файлы не используются другими тикетами).

**Ответ (200 OK):**
```json
{
  "status": "ok"
}
```

#### 3. Получить историю сообщений тикета
`GET /api/v1/admin/support/conversations/{id}/messages`

**Ответ (200 OK):**
```json
{
  "messages": [
    {
      "id": "msg-9876-uuid",
      "conversation_id": "conv-12345-uuid",
      "sender_role": "client",
      "text": "Здравствуйте, оплатил подписку по СБП, но она не пришла.",
      "created_at": "2026-08-07T21:46:56Z",
      "read_at": "2026-08-07T21:50:00Z"
    }
  ]
}
```

#### 3. Отправить сообщение клиенту
`POST /api/v1/admin/support/conversations/{id}/messages`

**Тело запроса (JSON):**
```json
{
  "text": "Подписка выдана, извините за задержку."
}
```

**Ответ (200 OK):** Возвращает созданный объект сообщения.

#### 4. Изменить статус тикета
`PATCH /api/v1/admin/support/conversations/{id}/status`

**Тело запроса (JSON):**
```json
{
  "status": "closed" 
}
```

**Ответ (200 OK):** Пустое тело (статус код 200).

#### 5. Заблокировать пользователя в саппорте
`POST /api/v1/admin/support/bans`

Блокирует пользователя только в системе саппорта (не затрагивая VPN-подписку или финансовый баланс). Автоматически разрывает активные WebSocket-сессии заблокированного клиента.

**Тело запроса (JSON):**
```json
{
  "user_id": "telegram:123456789",
  "reason": "Спам и оскорбления в тикетах",
  "expires_at": "2026-08-20T12:00:00Z"
}
```
*(Поле `expires_at` опционально. Если оно не указано, блокировка считается бессрочной).*

**Ответ (201 Created):**
```json
{
  "id": "ban-12345-uuid",
  "user_id": "telegram:123456789",
  "reason": "Спам и оскорбления в тикетах",
  "banned_by": "admin-id",
  "expires_at": "2026-08-20T12:00:00Z",
  "created_at": "2026-08-14T12:00:00Z",
  "updated_at": "2026-08-14T12:00:00Z"
}
```

#### 6. Разблокировать пользователя в саппорте
`DELETE /api/v1/admin/support/bans/{user_id}`

Снимает блокировку в саппорте с указанного пользователя.

**Ответ (200 OK):**
```json
{
  "status": "ok"
}
```

#### 7. Получить список всех активных блокировок в саппорте
`GET /api/v1/admin/support/bans`

**Ответ (200 OK):**
```json
{
  "bans": [
    {
      "id": "ban-12345-uuid",
      "user_id": "telegram:123456789",
      "reason": "Спам",
      "banned_by": "admin-id",
      "expires_at": null,
      "created_at": "2026-08-14T12:00:00Z",
      "updated_at": "2026-08-14T12:00:00Z"
    }
  ]
}
```

#### 8. Получить статус блокировки конкретного пользователя
`GET /api/v1/admin/support/bans/{user_id}`

**Ответ (200 OK):** Возвращает объект `SupportBan` или `404 Not Found`, если пользователь не заблокирован.

#### 9. Администраторский WebSocket (Real-time)
`GET /api/v1/admin/support/ws`

Администратор может подписаться на глобальный поток событий.
**Событие создания нового тикета клиентом:**
```json
{
  "type": "new_conversation",
  "payload": {
    "conversation_id": "conv-12345-uuid",
    "user_id": "123456789",
    "subject": "Проблема с оплатой",
    "created_at": "2026-08-07T21:46:56Z"
  }
}
```

**Событие нового сообщения от любого клиента:**
```json
{
  "type": "new_message",
  "payload": {
    "id": "msg-...",
    "conversation_id": "conv-12345-uuid",
    "sender_role": "client",
    "text": "А почему не работает?",
    "created_at": "...",
    "read_at": null
  }
}
```

