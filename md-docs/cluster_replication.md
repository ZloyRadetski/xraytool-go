# Репликация кластера

`cluster_replication` — единственный механизм репликации master/slave в xraytool. Он заменяет удалённый плагин `cluster_sync` и HTTP-эндпоинты `/api/v1/internal/xray/sync`.

Master — единственный источник истины. Slave устанавливают с ним постоянное gRPC-соединение с взаимной аутентификацией TLS 1.3. Каждый slave сохраняет запись во входящем журнале до подтверждения события, поэтому при переподключении безопасно получает неподтверждённые данные повторно. Для первоначальной и принудительной синхронизации используется потоковый снимок: каждый пользователь и артефакт конфигурации передаётся отдельным кадром, а не огромным JSON-запросом.

## Переход на новую схему

Обновите xraytool до одной версии на всех узлах, установите сертификаты, обновите конфигурацию каждого узла, затем запустите master и slave. Режима совместимости со старой HTTP-синхронизацией нет. Наличие `master_api`, `slave_api`, `slave_servers` или `plugins.cluster_sync` приведёт к ошибке загрузки конфигурации.

Сохраните резервную копию старой БД до подтверждения успешного перехода. Старые таблицы `sync_events` и `sync_states` автоматически не удаляются: новый плагин их не использует, но их сохранение предотвращает разрушительную миграцию.

## Конфигурация

На master:

```yaml
mode: master
replication:
  enabled: true
  node_id: master-1
  listen_address: "0.0.0.0:9443"
  allowed_nodes: ["slave-ru-1", "slave-eu-1"]
  ca_file: "/etc/xraytool/replication/ca.pem"
  cert_file: "/etc/xraytool/replication/master.pem"
  key_file: "/etc/xraytool/replication/master-key.pem"
  master_scan_interval: "30s"
  stats_interval: "30s"
  drift_interval: "1m"
```

На slave:

```yaml
mode: slave
replication:
  enabled: true
  node_id: slave-ru-1
  master_address: "master.example.com:9443"
  server_name: "master.example.com" # необязательное переопределение SNI
  ca_file: "/etc/xraytool/replication/ca.pem"
  cert_file: "/etc/xraytool/replication/slave-ru-1.pem"
  key_file: "/etc/xraytool/replication/slave-ru-1-key.pem"
  reconnect_interval: "5s"
  drift_interval: "1m"
  stats_interval: "30s"
```

## Антифрод между узлами

Событие подключения на slave сначала записывается в локальную устойчивую очередь, а затем передаётся по уже установленному mTLS gRPC-потоку. Master подтверждает событие только после обработки антифрод-плагином и записи idempotency-квитанции. При обрыве потока, перезапуске процесса или потерянном подтверждении slave повторяет отправку; повтор не увеличивает число IP. Сырые IP-адреса в репликацию не попадают.

На **master и каждом slave с `report_to_master: true`** должен быть задан один и тот же непустой `anti_fraud.salt_secret` (или `plugins.antifraud.config.salt_secret`, если используется новая секция плагина):

```yaml
anti_fraud:
  report_to_master: true # только на slave
  salt_secret: "вставьте-один-и-тот-же-секрет-на-все-узлы"
```

Сгенерируйте значение один раз на защищённой машине: `openssl rand -base64 32`. Не добавляйте его в Git и не используйте стандартное пустое значение. Адрес нормализуется перед HMAC-хешированием, поэтому `203.0.113.7` и `::ffff:203.0.113.7` считаются одним клиентом. В выводе `xraytool antifraud ips` и в API-снимке есть безопасное поле `hash_key_id`; оно должно совпадать на master и slave. Если slave прислал событие с другим идентификатором ключа, master не подтвердит его и в лог попадёт явная ошибка рассинхронизации вместо тихого двойного подсчёта.

Common Name (CN) сертификата каждого slave должен в точности совпадать с его `node_id`, а master должен содержать это значение в `allowed_nodes`. Файлы сертификата и ключа должны быть доступны для чтения только учётной записи, от которой работает xraytool.

## Выпуск сертификатов

Для репликации используйте собственный закрытый CA: публичный сертификат Let's Encrypt не подходит для аутентификации slave-узлов. Создавайте и храните закрытый ключ CA на рабочем месте администратора или выделенном CA-сервере, но никогда не на production-узле xraytool. Идентификаторы ниже соответствуют примеру конфигурации в этом репозитории.

### Windows (PowerShell)

Выполните эти команды в PowerShell на своём Windows-компьютере. Они используют OpenSSL из Git for Windows. Если файла `C:\Program Files\Git\usr\bin\openssl.exe` нет, установите Git for Windows либо OpenSSL for Windows, затем укажите фактический путь к `openssl.exe` в первой строке.

```powershell
$openssl = 'C:\Program Files\Git\usr\bin\openssl.exe'
if (-not (Test-Path -LiteralPath $openssl)) { throw 'openssl.exe не найден' }
& $openssl version

$certDir = Join-Path $env:USERPROFILE 'xraytool-ca'
New-Item -ItemType Directory -Force -Path $certDir | Out-Null
Set-Location $certDir

& $openssl genrsa -out ca-key.pem 4096
& $openssl req -x509 -new -sha256 -days 3650 -key ca-key.pem -out ca.pem -subj '/CN=xraytool-replication-ca'

@"
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=DNS:hokey.tvaldsforge.online
"@ | Set-Content -Path master.ext -Encoding ascii
& $openssl genrsa -out nld-master-key.pem 3072
& $openssl req -new -key nld-master-key.pem -out nld-master.csr -subj '/CN=nld-master'
& $openssl x509 -req -sha256 -days 825 -in nld-master.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -out nld-master.pem -extfile master.ext

@"
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=clientAuth
"@ | Set-Content -Path client.ext -Encoding ascii
& $openssl genrsa -out msk-slave-key.pem 3072
& $openssl req -new -key msk-slave-key.pem -out msk-slave.csr -subj '/CN=msk-slave'
& $openssl x509 -req -sha256 -days 825 -in msk-slave.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -out msk-slave.pem -extfile client.ext

& $openssl genrsa -out fld-slave-key.pem 3072
& $openssl req -new -key fld-slave-key.pem -out fld-slave.csr -subj '/CN=fld-slave'
& $openssl x509 -req -sha256 -days 825 -in fld-slave.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -out fld-slave.pem -extfile client.ext

& $openssl verify -CAfile ca.pem nld-master.pem msk-slave.pem fld-slave.pem
```

В итоге сертификаты будут в `%USERPROFILE%\xraytool-ca`. `ca-key.pem` и файлы `*-key.pem` никому не отправляйте и не добавляйте в Git.

### Linux/macOS

```bash
# Выполняется на защищённой машине CA.
umask 077
openssl genrsa -out ca-key.pem 4096
openssl req -x509 -new -sha256 -days 3650 -key ca-key.pem -out ca.pem \
  -subj "/CN=xraytool-replication-ca"

# Сертификат master обязан содержать точное публичное DNS-имя,
# которое используют slave.
openssl genrsa -out nld-master-key.pem 3072
openssl req -new -key nld-master-key.pem -out nld-master.csr \
  -subj "/CN=nld-master" -addext "subjectAltName=DNS:hokey.tvaldsforge.online"
printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:hokey.tvaldsforge.online\n' > master.ext
openssl x509 -req -sha256 -days 825 -in nld-master.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out nld-master.pem -extfile master.ext

# Выпустите отдельный клиентский сертификат для каждого slave.
# Его CN должен совпадать с node_id.
openssl genrsa -out msk-slave-key.pem 3072
openssl req -new -key msk-slave-key.pem -out msk-slave.csr -subj "/CN=msk-slave"
printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature\nextendedKeyUsage=clientAuth\n' > client.ext
openssl x509 -req -sha256 -days 825 -in msk-slave.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out msk-slave.pem -extfile client.ext

# Повторите предыдущие три команды, заменив msk-slave на fld-slave.
```

Скопируйте `ca.pem` и соответствующую пару сертификат/ключ в `/etc/xraytool/replication/` на каждый узел. Закрытый ключ должен иметь права `0600` и принадлежать учётной записи xraytool. Никогда не копируйте `ca-key.pem` на сервер. После установки файлов и ограничения доступа к TCP-порту `9443` IP-адресами slave измените `replication.enabled` на `true` на всех узлах и перезапустите xraytool.

## Работа и команды

- Изменения в движке на master создают компактные постоянные записи исходящего журнала. Периодическая проверка целевого состояния дополнительно обнаруживает изменения БД, сделанные вне движка, и создаёт маркер потокового переснимка. Master хранит события, пока каждый настроенный slave не подтвердит их получение; отставший узел, для которого история уже недоступна, получит новый потоковый снимок.
- Статические клиенты шаблона и ключи Reality — артефакты конфигурации. Они передаются только по mTLS-потоку; ключи Reality на slave записываются атомарно с правами `0600`.
- Статические (захардкоженные) пользователи шаблона передаются как профили пользователей, а не как копия inbound по тегу master. Поэтому теги и даже наборы inbound на узлах могут различаться: каждый такой пользователь добавляется во все совместимые локальные inbound каждого узла. Неподписанные записи без `email` не считаются пользователями и сохраняются только при точном совпадении локального тега и протокола.
- Статические клиенты применяются через единственный движок, который поддерживает эту возможность (обычно `engine_xray`). При двух таких движках репликация намеренно не публикует неоднозначный артефакт: сначала нужно ввести отдельный идентификатор движка в протокол.
- Суммарный трафик slave передаётся по тому же потоку и сохраняется на master по `node_id`, поэтому статистика кластера больше не опрашивает удалённый HTTP-эндпоинт.
- Slave сохраняет целевое состояние пользователей. Его цикл обнаружения расхождений восстанавливает управляемое состояние движка из этой проекции: вручную удалённый пользователь в одном inbound будет восстановлен, а правки на slave никогда не попадут обратно на master.
- На master доступны команды `xraytool plugin run cluster_replication status` и `xraytool plugin run cluster_replication snapshot`. `snapshot` добавляет компактный маркер, а подключённые slave сами получают развёрнутый потоковый снимок.
