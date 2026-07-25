# Исходящие вебхуки

Пакет `internal/events`. Вебхуки — основной способ уведомить внешние системы (Telegram-бот,
сайт, CRM) о событиях бэкенда.

## Настройка

```yaml
webhooks:
  - "https://bot.example.com/hooks/xraytool"
  - "https://crm.example.com/api/vpn-events"
webhook_secret: "длинная_случайная_строка"
```

Если список пуст, диспетчер ничего не отправляет и не тратит ресурсы.

## Формат запроса

```http
POST /hooks/xraytool HTTP/1.1
Content-Type: application/json
X-Webhook-Signature: 3f1a…    # HMAC-SHA256(тело, webhook_secret), hex; только если задан секрет

{
  "event_id": "evt_9f2c1c0d8b7a6e5f",
  "event_type": "subscription.expired",
  "timestamp": "2026-07-25T12:00:00Z",
  "data": { "...": "зависит от типа события" },
  "user_metadata": { "telegram_id": 123456789, "source": "telegram_bot" }
}
```

`user_metadata` — копия поля `users.metadata`, чтобы получатель мог сопоставить событие со
своим пользователем без дополнительного запроса.

### Проверка подписи (пример на Python)

```python
import hmac, hashlib
expected = hmac.new(secret.encode(), raw_body, hashlib.sha256).hexdigest()
if not hmac.compare_digest(expected, request.headers["X-Webhook-Signature"]):
    abort(401)
```

## Доставка и ретраи

* Отправка асинхронная, таймаут HTTP-клиента — 10 секунд.
* Успех — любой код `2xx`.
* До 4 попыток с задержками 2с → 4с → 8с; после этого событие теряется (запись в лог с
  уровнем `error`).
* При graceful shutdown сервер дожидается завершения всех незавершённых доставок
  (`Dispatcher.Shutdown`).

Получатель обязан быть **идемпотентным**: используйте `event_id` для дедупликации.

## Каталог событий

| `event_type` | Когда возникает | Ключевые поля `data` |
| --- | --- | --- |
| `auth.request_code` | Запрошен код входа (`POST /api/v1/users/request_code`) | `telegram_id`, `code` |
| `payment.completed` | Платёж переведён в статус `completed` | `payment_id`, `user_id`, `amount`, `payment_type`, `method` |
| `referral.reward` | Начислено реферальное вознаграждение | `referrer_id`, `referred_id`, `payment_id`, `amount` |
| `platega.callback` | Получен колбэк от Platega (сырое тело) | поля провайдера |
| `subscription.expiring` | Сработал порог `worker.expiration_warnings` | `user_id`, `subscription_id`, `email`, `warning_level`, `time_left_sec`, `ends_at` |
| `subscription.expired` | Подписка истекла и пользователь снят с движка | `user_id`, `subscription_id`, `email` |
| `device.limit_reached` | Превышен лимит устройств при выдаче подписки | `email`, `client_id`, `hwid`, `device_limit`, `device_model`, `device_os`, `user_agent` |
| `file.uploaded` | Загружен файл через `/api/rest/upload` | `path` |

Предупреждения об истечении отправляются один раз на каждый порог: факт отправки
фиксируется в таблице `subscription_notifications`.

## Входящий вебхук Platega

`POST /api/v1/payments/platega/callback` — колбэк платёжного провайдера. Проверяются
заголовки merchant ID и секрета (`platega_merchant_id`, `platega_secret`); при успешной
оплате платёж завершается, подписка продлевается, начисляется реферальное вознаграждение
и рассылается событие `payment.completed`. Подробности — в
[api_documentation.md](api_documentation.md).

## Отладка

Быстрый приёмник для проверки формата:

```bash
python3 -m http.server 9000 &        # или любой инструмент вроде webhook.site
# в config.yaml: webhooks: ["http://127.0.0.1:9000/hook"]
```

В логах ищите строки с префиксом `[EVENT_DISPATCHER]` — они содержат `event_id`, URL,
номер попытки и итог доставки.
