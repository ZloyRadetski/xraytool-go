# ⚡ xraytool-go

**xraytool-go** — это система управления распределенным кластером VPN-узлов на базе **Xray-core** (VLESS, Hysteria2, VMess, Shadowsocks, Trojan) с архитектурой **Master-Slave** и поддержкой динамической ротации ключей Reality.

Проект предназначен для централизованной синхронизации пользователей, автоматического поддержания актуальных ключей безопасности, управления лимитами подключений и предоставления гибкого API подписок.

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

#### Пример `docker-compose.yml`
```yaml
version: '3.8'

services:
  xraytool_backend:
    image: xraytool_backend:latest
    container_name: xraytool_backend
    restart: always
    volumes:
      - ./data:/etc/xraytool
      - ./xray_config:/usr/local/etc/xray
      - /var/run/docker.sock:/var/run/docker.sock
    ports:
      - "8080:8080"
    environment:
      - APP_ENV=production
    depends_on:
      - xray_core

  xray_core:
    image: teddysun/xray:latest
    container_name: xray_core
    restart: always
    volumes:
      - ./xray_config:/usr/local/etc/xray
      - ./logs:/var/log/xray
    ports:
      - "443:443"
      - "10085:10085" # gRPC API Порт
```

### 3. Настройка конфигурации (`config.yaml`)
В каталоге `/etc/xraytool/` создайте файл конфигурации `config.yaml`:

```yaml
# Режим работы текущей ноды: "master" или "slave"
mode: master

# Настройки Reality-ключей (ротация только на Master)
reality:
  rotation_enabled: true
  keys_filepath: "/etc/xraytool/configs/reality.keys"

# Настройки связи Master -> Slave
slave_api:
  connect_timeout: 3s
  request_timeout: 30s
  remote_path: "/api/v1/internal/xray/sync"
```

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
   xraytool syncstates
   ```
