# Развертывание

## 1. Требования

| Компонент | Требование |
| --- | --- |
| ОС | Linux x86_64 (Debian/Ubuntu), systemd |
| Права | root (правка конфигов Xray, управление сервисами) |
| Go | версия из `go.mod` (только для сборки из исходников) |
| Xray-core | установлен и запущен, включён gRPC API |
| Hysteria 2 | опционально, если используется соответствующий inbound |
| БД | SQLite (по умолчанию) или PostgreSQL 13+ |
| Прочее | синхронизированное системное время (chrony / systemd-timesyncd) на **всех** узлах |

## 2. Вариант A — Docker (рекомендуется)

```bash
mkdir -p ~/xraytool/data && cd ~/xraytool
# положите рядом docker-compose.yml из репозитория
docker compose up -d
```

Что делает `docker-compose.yml`:

* поднимает `ghcr.io/zloyradetski/xraytool-go:latest` c `network_mode: host` и
  `privileged: true` (нужно для управления Xray на хосте);
* монтирует `./data` в `/etc/xraytool`, конфиг Xray, `/dev/shm`, логи и конфиг Hysteria;
* добавляет **Watchtower**, который каждые 300 секунд проверяет обновление образа и
  удаляет старые слои.

Внутри образа `systemctl` и `xray` — это обёртки поверх `nsenter`, выполняющиеся в
пространстве имён хоста, поэтому контейнер управляет системными сервисами напрямую.

Точка входа — `./xraytool start-server`; CLI-команды выполняются через `docker exec`
(см. [docker_management.md](docker_management.md)).

## 3. Вариант B — бинарник + systemd

```bash
git clone https://github.com/ZloyRadetski/xraytool-go.git && cd xraytool-go
make build-linux            # CGO_ENABLED=0, статический бинарник в ./build
sudo install -m 0755 build/xraytool-linux-amd64 /usr/local/bin/xraytool
# либо просто: sudo make install
```

Юнит `/etc/systemd/system/xraytool.service`:

```ini
[Unit]
Description=xraytool backend
After=network-online.target xray.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/xraytool start-server --config /etc/xraytool/config.yaml
Restart=always
RestartSec=5
User=root
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now xraytool
sudo journalctl -u xraytool -f
```

## 4. Подготовка Xray-core

1. Установите ядро: `xraytool update-xray` (или официальный установщик).
2. Обновите базы маршрутизации: `xraytool update-geo`.
3. В `config.json` (или в `xray_template.json`, из которого он собирается) включите gRPC API
   и access-лог:

```json
{
  "log":   { "access": "/dev/shm/xray-access.log", "loglevel": "warning" },
  "api":   { "tag": "api", "services": ["HandlerService", "StatsService", "LoggerService"] },
  "stats": {},
  "policy": { "system": { "statsInboundUplink": true, "statsInboundDownlink": true } },
  "inbounds": [
    { "tag": "api", "listen": "127.0.0.1", "port": 10085, "protocol": "dokodemo-door", "settings": { "address": "127.0.0.1" } }
  ],
  "routing": { "rules": [ { "type": "field", "inboundTag": ["api"], "outboundTag": "api" } ] }
}
```

Адрес должен совпадать с `xray.api_addr` в `config.yaml`. Без gRPC API xraytool сможет
работать только через правку конфига с рестартом ядра (медленно и с разрывом сессий).

## 5. Настройка Master

```bash
sudo mkdir -p /etc/xraytool/configs
sudo xraytool userlist          # первый запуск создаст /etc/xraytool/config.yaml
sudo nano /etc/xraytool/config.yaml
```

Минимум, что нужно указать: `mode: master`, `server.ip`, `server.domain`,
`server.api_key` (длинная случайная строка), пути и настройки БД. Полный справочник —
[configuration.md](configuration.md).

Далее:

```bash
sudo xraytool rebuild-config     # собрать config.json из шаблона + БД + Reality-ключи
sudo systemctl restart xray
sudo systemctl enable --now xraytool
```

API слушает `127.0.0.1:8080`. Наружу его публикуют через reverse-proxy с TLS:

```nginx
server {
    listen 443 ssl http2;
    server_name vpn.example.com;

    ssl_certificate     /etc/letsencrypt/live/vpn.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/vpn.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Заголовок `X-Real-IP`/`X-Forwarded-For` важен: по нему определяется IP клиента при выдаче
подписки.

## 6. Настройка Slave

На новом сервере:

1. установите Docker или бинарник и Xray-core (шаги 2–4);
2. скопируйте с мастера `xray_template.json` и `configs.txt`;
3. создайте `config.yaml`:

```yaml
mode: slave
server:
  ip: "203.0.113.20"
  domain: "de.example.com"
  api_key: "ключ_этого_слейва"     # должен совпадать со slave_servers.<node>.api_key на мастере
master_api:
  url: "https://vpn.example.com/api/v1/internal/xray/sync"
  api_key: "ключ_мастера"          # равен server.api_key мастера
anti_fraud:
  enabled: true
  report_to_master: true
  salt_secret: "общая_соль_кластера"
reality:
  rotation_enabled: false          # ключи присылает мастер
```

4. запустите: `sudo systemctl enable --now xraytool`;
5. на мастере добавьте узел в `slave_servers` и выполните полную синхронизацию:

```bash
sudo nano /etc/xraytool/config.yaml     # добавить slave_servers.<node>
sudo systemctl restart xraytool
sudo xraytool syncstates --full
```

Проверка:

```bash
curl -s -H "X-API-Key: $MASTER_KEY" https://vpn.example.com/api/v1/internal/xray/sync/state
sudo xraytool syncstates --dry-run       # ожидаем match для всех узлов
```

Подробности протокола — [cluster_sync.md](cluster_sync.md).

## 7. Чек-лист после развертывания

- [ ] `server.api_key` заменён (не `CHANGE_ME_IN_CONFIG`), длина ≥ 32 символов
- [ ] API не доступен снаружи напрямую (только через reverse-proxy с TLS)
- [ ] Время синхронизировано на всех узлах: `timedatectl status`
- [ ] Access-лог антифрода лежит в `/dev/shm`, `anti_fraud.salt_secret` одинаков везде
- [ ] `webhook_secret` задан, приёмник проверяет `X-Webhook-Signature`
- [ ] Настроено резервное копирование БД и `reality.keys` (см. [database_schema.md](database_schema.md))
- [ ] `xraytool cli-stats` и `xraytool userlist` отрабатывают без ошибок
- [ ] Тестовая подписка выдаётся: `curl -A "happ/1.0" "https://<domain>/client?id=<uuid>&hwid=test"`
