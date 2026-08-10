# Автобалансеры в JSON-подписке

Версия `2` шаблона подписки позволяет один раз описать сервер и использовать
его одновременно как обычный профиль подписки и как участника Xray
автобалансера. Шаблон — это исходный документ: перед отдачей пользователю
`xraytool` компилирует его в обычный массив конфигураций Xray. Поля `version`,
`servers` и `subscription` в клиентский JSON не попадают.

За обработку v2 отвечает builtin-плагин `subscription_autobalancer`. В
стандартной конфигурации он включён. Его можно заменить совместимым плагином,
публикующим сервис `subscription_template_processor`; при выключенном плагине
v2-шаблоны не используются, а legacy JSON-массивы продолжают работать.

Старый формат — JSON-массив профилей — остаётся полностью совместимым.

## Пример

```json
{
  "version": 2,
  "servers": {
    "nl-1": {
      "name": "🇳🇱 Netherlands",
      "outbound": {
        "protocol": "vless",
        "settings": {
          "vnext": [{
            "address": "nl.example.com",
            "port": 443,
            "users": [{ "id": "{UUID}", "encryption": "none" }]
          }]
        },
        "streamSettings": { "network": "tcp", "security": "reality" }
      }
    },
    "de-1": {
      "name": "🇩🇪 Germany",
      "outbound": {
        "protocol": "vless",
        "settings": {
          "vnext": [{
            "address": "de.example.com",
            "port": 443,
            "users": [{ "id": "{UUID}", "encryption": "none" }]
          }]
        }
      }
    }
  },
  "subscription": [
    { "type": "server", "ref": "nl-1" },
    { "type": "server", "ref": "de-1" },
    {
      "type": "auto_balancer",
      "id": "eu-auto",
      "name": "🌍 Europe · Auto",
      "members": [{ "ref": "nl-1" }, { "ref": "de-1" }],
      "probe": {
        "url": "https://www.gstatic.com/generate_204",
        "interval": "1m",
        "timeout": "3s",
        "sampling": 1
      },
      "strategy": {
        "type": "leastLoad",
        "baselines": ["1s"],
        "expected": 2,
        "max_rtt": "1s",
        "tolerance": 0.1
      },
      "fallback": "direct"
    }
  ]
}
```

`servers` — каталог серверов, а `subscription` определяет, что увидит
клиент. Идентификаторы серверов и балансеров должны состоять из латинских
букв, цифр, `.`, `_` и `-`.

## Серверы только для балансера

Участник балансера не обязан быть отдельным элементом подписки. Его можно
задать прямо в `members`; у него всё равно должен быть стабильный `id`:

```json
{
  "version": 2,
  "subscription": [{
    "type": "auto_balancer",
    "id": "north-backup",
    "name": "North backup · Auto",
    "members": [
      {
        "server": {
          "id": "fi-backup",
          "name": "🇫🇮 Finland Backup",
          "outbound": { "protocol": "vless", "settings": { "vnext": [] } }
        }
      },
      {
        "server": {
          "id": "se-backup",
          "name": "🇸🇪 Sweden Backup",
          "outbound": { "protocol": "vless", "settings": { "vnext": [] } }
        }
      }
    ]
  }]
}
```

В реальном шаблоне `outbound` должен быть полноценным proxy-outbound Xray.
Минимум два участника обязательны.

Если уже есть полный профиль Xray, вместо `outbound` допускается `config`.
Такой профиль должен содержать ровно один proxy-outbound либо поле
`outbound_tag`, выбирающее один из нескольких.

## Выдача в разных форматах

JSON-подписка получает отдельные обычные профили и нативный Xray-профиль
балансера с `burstObservatory`, `routing.balancers` и изолированными тегами
членов.

VLESS и Clash не содержат Xray routing-балансер. При их генерации:

- сам балансер не экспортируется;
- сервер, уже выданный как обычный профиль, не дублируется;
- сервер, существующий только внутри балансера, экспортируется один раз;
- для готового JSON без исходного v2-шаблона используется дополнительная
  дедупликация по нормализованному endpoint (без Xray `tag`).

## Проверка до развёртывания

```powershell
xraytool --config config.yaml subscription validate --input configs_v2.json
xraytool --config config.yaml subscription render --input configs_v2.json --format json
xraytool --config config.yaml subscription render --input configs_v2.json --format vless
xraytool --config config.yaml subscription render --input configs_v2.json --format clash
```

Команды только читают шаблон и ничего не записывают. Валидация проверяет
ссылки, повторяющиеся ID, структуру outbound, минимальное число участников и
поддерживаемую стратегию `leastLoad`.
