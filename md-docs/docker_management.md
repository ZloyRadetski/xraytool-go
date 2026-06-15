# Инструкция по управлению Docker

Эта документация содержит все необходимые команды для развертывания, обновления и управления бэкендом `xraytool-go` через Docker.

## 1. Базовые команды управления

Все команды выполняются из папки, где лежит `docker-compose.yml` (обычно `~/xraytool`).

* **Просмотр логов (в реальном времени):**
  ```bash
  docker compose logs -f backend
  ```
* **Обычная перезагрузка бэкенда:**
  ```bash
  docker compose restart backend
  ```
* **Узнать размер контейнера:**
  ```bash
  docker images | grep xraytool-go
  ```

---

## 2. Обновление контейнера

Бэкенд обновляется автоматически (каждые 5 минут) с помощью контейнера **Watchtower**. 
Но если вы хотите принудительно обновить бэкенд прямо сейчас, выполните:

```bash
docker compose pull backend && docker compose up -d
```
Эта команда скачает свежую версию с GitHub (если она есть) и мгновенно пересоздаст контейнер, сохранив все данные.

---

## 3. Выполнение команд внутри контейнера

Поскольку `xraytool` работает в изолированной среде Docker, консольные команды нужно передавать внутрь.

* **Единоразовое выполнение команды:**
  ```bash
  docker exec -it xraytool_backend ./xraytool [команда]
  ```
  *(Например: `docker exec -it xraytool_backend ./xraytool newuser test@email.com`)*

* **Создание удобного ярлыка на сервере:**
  Чтобы не писать `docker exec` каждый раз, создайте алиас-переходник:
  ```bash
  echo '#!/bin/bash' | sudo tee /usr/local/bin/xraytool
  echo 'docker exec -it xraytool_backend ./xraytool "$@"' | sudo tee -a /usr/local/bin/xraytool
  sudo chmod +x /usr/local/bin/xraytool
  ```
  Теперь вы можете просто писать `xraytool newuser test@email.com` в консоли сервера.

---

## 4. Миграция базы данных (db-migrate)

**Важно:** Контейнер не видит файлы на вашем хост-сервере. Он видит только то, что лежит в папке `~/xraytool/data` (внутри контейнера она называется `/etc/xraytool`).

**Правильный алгоритм миграции старой базы:**
1. Скопируйте старый файл базы в проброшенную папку:
   ```bash
   cp /путь/к/старой/bot.db ~/xraytool/data/
   ```
2. Запустите миграцию, указывая пути **внутри контейнера** (`/etc/xraytool/...`):
   ```bash
   xraytool db-migrate --from /etc/xraytool/bot.db --devices /etc/xraytool/devices_state.json
   ```

---

## 5. Деплой на Slave-сервера

Масштабирование бэкенда не требует установки Go. Все slave-ноды работают на Docker и общаются с Мастером по защищенному API.

**Шаги для нового сервера:**
1. Установите Docker:
   ```bash
   curl -fsSL https://get.docker.com -o get-docker.sh && sudo sh get-docker.sh
   ```
2. Создайте нужные папки:
   ```bash
   mkdir -p ~/xraytool/data
   cd ~/xraytool
   ```
3. Скопируйте 3 файла с Master-сервера:
   * `docker-compose.yml` ➡️ в папку `~/xraytool/`
   * `data/config.yaml` ➡️ в папку `~/xraytool/data/` (Измените в нем `mode: master` на `mode: slave`)
   * `data/xray_api_config.json` ➡️ в папку `~/xraytool/data/` (В нем **обязан** быть тот же `api_key`, что и на мастере!).
4. Запустите:
   ```bash
   docker compose up -d
   ```
5. Вернитесь на Master-сервер и добавьте IP нового слейва в файл `~/xraytool/data/servers.json`:
   ```json
   {
     "servers": [
       "http://IP_НОВОГО_СЛЕЙВА:8080"
     ]
   }
   ```
   *(Master сам начнет синхронизировать юзеров с этой нодой).*
