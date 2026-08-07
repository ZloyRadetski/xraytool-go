# Разработка плагинов xraytool

Плагин — изолированный компонент, который объявляет метаданные, зависимости и
жизненный цикл через `internal/pluginapi`. Ядро строит граф зависимостей до
инициализации любого компонента и запускает плагины в топологическом порядке.

## Конфигурация

Плагины и движки объявляются в `xraytool.yml`:

```yaml
plugins:
  payment_platega:
    enabled: true
    source: builtin
    config:
      merchant_id: "merchant-id"
      secret: "secret"

  payment_example:
    enabled: true
    source: external
    exec: /opt/xraytool/plugins/payment-example
    manifest: /opt/xraytool/plugins/payment-example/plugin.yaml
    # Optional; otherwise $XRAYTOOL_PLUGIN_LOG_DIR or the OS cache directory.
    log_path: /var/log/xraytool/plugins/payment-example.log
    args: ["--verbose"]
    restart_policy:
      max_restarts: 5
      backoff: 2s
    config:
      api_key: "..."

engines:
  routing_mode: by-plan
  xray:
    enabled: true
    source: builtin
  singbox:
    enabled: true
    source: builtin
    config:
      config_path: /etc/sing-box/config.json
      managed_inbound_tags: [vless-in]
      check_command: [sing-box, check]
      reload_command: [systemctl, reload, sing-box]
```

`core` обязателен и не может быть выключен. Для каждого включённого плагина
нужен `source`: `builtin`, `external` с полем `exec`, либо
`external:/абсолютный/путь`. Для external-плагина рекомендуется указывать
`manifest`: он даёт командам CLI метаданные без запуска процесса и включает
проверку `config` по JSON Schema при загрузке конфигурации.

Встроенные плагины также имеют встроенные JSON Schema. Неизвестное поле,
неверный тип или обязательное отсутствующее поле отклоняются до запуска
сервера. Пути `manifest` и относительный `config_schema` разрешаются от файла
основной конфигурации и манифеста соответственно; сетевые URL для схем не
поддерживаются.

## Встроенный Go-плагин

Реализуйте `pluginapi.Plugin`; плагин, публикующий сервисы, также реализует
`pluginapi.ServiceProvider`.

```go
func (p *Plugin) Metadata() pluginapi.Metadata {
    return pluginapi.Metadata{
        Name:       "example",
        Kind:       "notification",
        Version:    "1.0.0",
        APIVersion: pluginapi.CurrentAPIVersion,
        Requires:   []pluginapi.ServiceRef{{Name: "subscription_repository"}},
        Publishes:  []pluginapi.ServiceRef{{Name: "example_service"}},
    }
}
```

`Init` разрешает только сервисы из `Requires`. Нельзя хранить
`ServiceResolver` и вызывать его в hot path: все зависимости нужно получить во
время `Init`. `Start` блокируется до отмены контекста, `Stop` освобождает
ресурсы, `Health` сообщает о состоянии без разрушительных операций.

Добавьте фабрику в `pluginhost.BuiltinRegistry` и манифест `plugin.yaml` рядом
с исходным кодом. Проверить манифест можно командой:

```text
xraytool plugin verify internal/plugins/example/plugin.yaml
```

Если плагин владеет данными, он реализует `pluginapi.MigrationProvider` и
возвращает embedded-набор SQL-миграций. Перед `Init` Host запускает их
транзакционно и ведёт отдельную таблицу `schema_migrations_<plugin>`. Плагин
получает только свой `pluginapi.PluginDBHandle`; общая `AutoMigrate` больше не
является способом изменять схему приложения.

## Внешний плагин

Внешний плагин использует пакет `xraytool/pluginrpc`. Он запускается через
HashiCorp go-plugin с взаимной TLS-аутентификацией и общим handshake. Минимальная
точка входа Go-плагина:

```go
package main

import "xraytool/pluginrpc"

func main() { pluginrpc.Serve(&implementation{}) }
```

`implementation` реализует `pluginrpc.Implementation`. Транспорт передаёт
только JSON-совместимые структуры: `Describe`, `Init`, `Start`, `Stop`,
`Health` и `Call`. Внешнему процессу нельзя передавать произвольные Go-объекты,
`*http.Request`, репозитории или `ServiceResolver`.

Если плагину требуется сервис хоста, он должен объявить его в `Requires`.
Хост создаёт brokered `ServiceProxy`; через него доступны только явно
зарегистрированные сериализуемые методы. Это ограничение намеренно: оно делает
границу процесса проверяемой и не превращает Plugin Host в удалённый service
locator.

Плагины внешних типов payment, notification и event sink получают адаптеры для
безопасных структурированных вызовов. Не сериализуемые extension points будут
отклонены при загрузке с понятной ошибкой.

## Антифрод и hot path

Запрос подписки никогда не делает RPC к антифрод-плагину. Провайдер передаёт
изменения банов в kernel через push sink, а `PluginHost` обслуживает запросы из
локального TTL-кэша. Внешний антифрод обязан публиковать эти изменения, а не
предполагать polling на каждом запросе. Снимок банов восстанавливается при
старте, а каждое последующее изменение приходит через отдельный push-канал;
Host ограничивает размер и TTL локального кэша. Это сохраняет подписочный hot
path локальным даже при перезапуске или задержке внешнего процесса.

## Несколько VPN-движков

`engine_xray` и `engine_singbox` могут работать одновременно. `broadcast`
обслуживает пользователя на всех включённых движках. Явный список движков в
метаданных подписки всегда имеет приоритет как административное исключение.
Без него `by-plan` использует `Plan.EngineIDs`, а
`by-subscription-override` использует план как совместимый fallback; при
отсутствии данных оба режима безопасно возвращаются к broadcast. Неизвестный ID
движка — ошибка, а не тихая потеря пользователя.

Sing-box управляет собственным JSON-файлом атомарно, проверяет его опциональной
`check_command` и вызывает заданную оператором `reload_command`. Он не запускает
ещё один неконтролируемый процесс Sing-box. Стандартный Sing-box не имеет
переносимого API статистики по пользователю: без `stats_endpoint` агрегированная
статистика этого движка помечается недоступной, тогда как остальные движки
продолжают обслуживаться.

## Операционные команды

```text
xraytool plugin list
xraytool plugin graph
xraytool plugin enable <name>
xraytool plugin disable <name>
xraytool plugin verify <manifest-path>
xraytool plugin verify <manifest-path> --exec /opt/xraytool/plugins/example
xraytool plugin logs <name> --tail 200
xraytool plugin logs <name> --follow
```

`plugin graph` проверяет зависимости без запуска плагинов. Изменения
`enable/disable` вступают в силу после перезапуска сервера. Политика
`restart_policy` относится только к external-плагинам: хост выполняет
ограниченное число перезапусков с указанной паузой и останавливает процесс при
graceful shutdown. Стандартный вывод и stderr external-плагина сохраняются в
ограниченном файле (2 MiB); путь задаётся `log_path`,
`XRAYTOOL_PLUGIN_LOG_DIR` или платформенным cache-dir. `plugin logs` читает
именно этот постоянный журнал, поэтому работает и из отдельного CLI-процесса.

## Минимальная сборка

`make build-minimal` собирает `xraytool` с тегом `minimal`. В таком бинаре
отсутствуют optional built-in antifraud, cluster transport, mailer, webhook,
Platega и Sing-box, а также их legacy-пакеты `slave`/`statesync`; остаются
обязательный core, pricing и Xray engine. Конфигурация, ссылающаяся на
исключенный builtin-плагин, будет отвергнута командой `plugin graph` или при
старте с ясным сообщением. Те же extension points можно предоставить внешним
процессом без включения optional-кода в бинарник.
