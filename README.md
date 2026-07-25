# xraytool-go

**xraytool-go** — это система управления распределенным кластером VPN-узлов на базе **Xray-core** (VLESS, Hysteria2, VMess, Shadowsocks, Trojan) с архитектурой **Master-Slave** и поддержкой легкого внедрения кастомных ядер.

Проект предназначен для централизованной синхронизации пользователей, автоматического поддержания актуальных ключей безопасности, управления лимитами подключений и предоставления гибкого API подписок, а также встроенного биллинга и платежных систем. 

[![Docker Build and Publish](https://github.com/ZloyRadetski/xraytool-go/actions/workflows/docker-publish.yml/badge.svg)](https://github.com/ZloyRadetski/xraytool-go/actions/workflows/docker-publish.yml)
---

## 📚 Документация

Полный набор документации — в каталоге [`md-docs/`](md-docs/README.md).

| Документ | О чём |
| --- | --- |
| [Архитектура](md-docs/architecture.md) | Слои, пакеты, потоки данных, роли Master/Slave |
| [Развертывание](md-docs/deployment.md) | Docker, бинарник, systemd, настройка Master и Slave |
| [Конфигурация](md-docs/configuration.md) | Полный справочник по `config.yaml` |
| [CLI](md-docs/cli_reference.md) | Все команды и флаги с примерами |
| [REST API](md-docs/api_documentation.md) | Эндпоинты с примерами запросов и ответов |
| [Синхронизация кластера](md-docs/cluster_sync.md) | Протокол ping / delta / full-sync, ротация Reality-ключей |
| [Подписки](md-docs/subscriptions.md) | Конвейер выдачи, HWID, лимиты устройств, шаблоны |
| [Антифрод](md-docs/antifraud.md) | Детект раздачи подписки и временные баны |
| [Схема БД](md-docs/database_schema.md) | Таблицы, связи, миграции, бэкапы |
| [Вебхуки](md-docs/webhooks.md) | Формат событий, HMAC-подпись, ретраи |
| [Разработка](md-docs/development.md) | Сборка, тесты, соглашения, CI |
| [Диагностика](md-docs/troubleshooting.md) | Типовые проблемы и их решения |

---

## 🛠️ Инструкция по развертыванию

### ⚠️ Важное системное требование (Синхронизация времени)
> [!IMPORTANT]
> **На всех серверах кластера (Master и Slaves) должно быть настроено абсолютно одинаковое системное время!**  
> Расхождение даже в несколько секунд может приводить к сбоям в протоколах gRPC-синхронизации, неверной проверке сроков действия подписок и ошибкам рукопожатия (handshake) у клиентов.  
> 
> Рекомендуется установить и запустить службу синхронизации времени (например, `chrony` или `systemd-timesyncd`):
> ```bash
> # Установка chrony на Ubuntu/Debian
> apt-get update && apt-get install -y chrony
> systemctl enable --now chrony
> # Проверка статуса синхронизации
> chronyc tracking
> ```

---

### 1. Сборка проекта
Для сборки исполняемого файла выполните команду в корне проекта:
```bash
go build -o bin/xraytool main.go
```

### 2. Запуск в Docker Compose (Рекомендуемый способ)
Разверните бэкенд xraytool-go совместно с Xray-core на каждом сервере.

#### Пример `docker-compose.yml` можете найти в корне проекта `docker-compose.yml`

### 3. Настройка конфигурации (`config.yaml`)
Каталог `./data` пробрасывается в контейнер как `/etc/xraytool`, поэтому конфигурация лежит в `./data/config.yaml`.
При первом запуске файл создаётся автоматически с закомментированными примерами всех параметров.
Полный справочник по секциям — [md-docs/configuration.md](md-docs/configuration.md).

### 4. Первоначальный запуск и синхронизация
1. Сгенерируйте локальный конфигурационный файл `config.json` для Xray-core на основе шаблона и базы данных:
   ```bash
   xraytool rebuild-config
   ```
2. Запустите ротацию Reality-ключей на Master-сервере (будут созданы новые ключи X25519 и Short ID):
   ```bash
   xraytool rotate-keys
   ```
3. Выполните синхронизацию со всеми подчиненными узлами (Slaves):
   ```bash
   xraytool syncstates --full
   ```

Подробная пошаговая инструкция (включая systemd, reverse-proxy и настройку Slave-нод) — в [md-docs/deployment.md](md-docs/deployment.md).
