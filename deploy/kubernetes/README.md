# MVP в существующем многоузловом Kubernetes на Ubuntu

Статус: код backend и генератор ресурсов подготовлены; развёртывание в настоящем
кластере ещё не проверено. Registry доступен — установка браузеров/шрифтов/Go
на узлах Ubuntu не требуется. В узлах остаются Kubernetes, CRI runtime, CNI,
CSI, доверие к registry CA и правила firewall. Образы не заменяют эти компоненты.

## Состав

- `mvp1-control`: один Pod, два контейнера gateway/runner, общий приватный Unix
  socket. Реплика одна, стратегия Recreate. Масштабирование gateway с SQLite
  и локальной картой активных worker пока не поддерживается.
- PVC SQLite: 1 GiB, Filesystem поверх подходящего block CSI StorageClass, RWO.
  Не использовать NFS для SQLite WAL. PVC должен переноситься между нужными
  узлами; `local-path` привязывает данные к узлу и не даёт такого failover.
- Динамические session Pods: Falkon / Firefox / terminal, Go worker, Xvfb,
  Openbox, FFmpeg, шрифты и клиентские библиотеки IPP уже в образе. Профили,
  `/tmp`, `/dev/shm` — ограниченные memory emptyDir, PDF — в памяти worker.
- coturn: отдельный namespace `<namespace>-turn`, один закреплённый узел,
  hostNetwork и стабильный адрес. Этот вариант требует исключения admission
  для hostNetwork именно у TURN; baseline namespace приложений не ослабляется.
- Один Pod на приложение, не Deployment: restartPolicy Never. После падения
  приложение завершается, пользователь запускает новый одноразовый сеанс.

Runner использует HTTPS Kubernetes API с CA кластера и обновляемым projected
ServiceAccount token. Токен монтируется только в runner, не gateway/приложения.
RBAC — get/list/create/delete Pods в выделенном namespace. Это полномочия на
все Pods namespace, label selector сам по себе не ограничивает RBAC; поэтому
не размещайте в этом namespace посторонние сервисы. Удаление проверяет владельца
и UID и ожидает исчезновения объекта. Не форсировать удаление при потере узла:
исчезновение объекта при force delete не доказывает завершения процесса.

## Обязательные входные параметры

Скопировать `config.example.json` в файл вне репозитория и заполнить:

- namespace (до 31 символа), адреса control/worker image с версией или digest;
- registry imagePullSecret в namespace приложений;
- StorageClass; домен HTTPS и доверенный сертификат;
- TURN URL, внешний IP, внутренний IP узла и его `kubernetes.io/hostname`;
- CIDR worker Pods на всех узлах и CIDR разрешённых внутренних веб-приложений.

`application_cidrs` открывает только TCP 80/443. Для иных портов измените правила
NetworkPolicy явно. CIDR не должны случайно включать API кластера, worker Pods,
metadata endpoints и другие запрещённые назначения. IP-правила не заменяют
проверку маршрутов и DNS. DNS-правило предполагает CoreDNS с меткой `k8s-app=kube-dns`
в kube-system; при NodeLocal DNS его нужно адаптировать.

Оператор должен подтвердить CNI с поддержкой NetworkPolicy, cluster Pod CIDR,
StorageClass и политику seccomp/AppArmor. Worker использует UID0 только для
управляющего процесса и SETUID/SETGID/KILL; сами приложения UID1000. Namespace
restricted без исключения отклонит эти Pods. Для Falkon проверить user namespaces
и sandbox QtWebEngine на каждом подходящем Ubuntu-узле. Автоматического
`--no-sandbox` нет. Лимит kubelet podPidsLimit должен позволять как минимум
512 задач для браузерного Pod; точный запас определяется испытанием.

## Образы

На машине сборки с Go из проекта и Docker:

```sh
./deploy/build-images.sh registry.company.example/mvp1 RELEASE
# После проверки образов:
docker push registry.company.example/mvp1/mvp1-control:RELEASE
docker push registry.company.example/mvp1/mvp1-worker:RELEASE
```

Узлы должны поддерживать архитектуру собранных бинарников/образов. Текущая
команда собирает для архитектуры машины сборки. Для ARM64 нужны соответствующая
сборка и проверки. Пакеты устанавливаются только при build; при запуске apt нет.
Базовый Debian закреплён digest, но apt-репозитории не snapshot: после сборки
фиксируйте конечные image digest и версии пакетов. Debian userland контейнера
нормально отделён от Ubuntu на узле; общим остаётся ядро Linux.
Генератор добавляет DaemonSet `mvp1-prepull` для предварительной загрузки worker image.
Дождитесь его готовности на всех целевых узлах перед допуском пользователей: первый pull большого
образа может превысить дедлайн запуска около 25 секунд и ухудшает пользовательский путь.

## Ресурсы и секреты

Сначала создать namespace приложений и `<namespace>-turn`, согласовать admission
для TURN и создать перечисленные ниже секреты. Затем сформировать и проверить ресурсы:

```sh
python3 deploy/kubernetes/render.py /path/to/config.json > /tmp/mvp1.json
kubectl apply --dry-run=server -f /tmp/mvp1.json
```

Необходимые секреты перечислены ниже. Генератор не содержит паролей и не создаёт секреты:

- в namespace приложений `mvp1-secrets` с ключами `admin-login`, `admin-password`,
  `turn-secret`; `mvp1-tls` типа kubernetes.io/tls; registry secret из config;
- в namespace TURN `mvp1-secrets` только с ключом `turn-secret` (то же значение),
  `mvp1-turn-tls` с сертификатом для TURN-домена.

Создавайте секреты из защищённых файлов/менеджера секретов, не передавайте пароли
в историю команд. Начальная учётная запись создаётся только для пустой БД.
Существующие пароли при повторном запуске не сбрасываются. Не копировать
`testing/data/mvp1.db` в рабочий PVC как часть образа.

После dry-run/admission review и создания секретов:

```sh
kubectl apply -f /tmp/mvp1.json
kubectl -n mvp1 rollout status daemonset/mvp1-prepull
kubectl -n mvp1 rollout status deployment/mvp1-control
kubectl -n mvp1-turn rollout status deployment/mvp1-turn
```

Имена namespace в последних командах заменить своими. Генератор не применяет
ресурсы сам и не меняет настройки узлов/кластера.

## HTTPS и WebRTC

Service `mvp1` — ClusterIP 443 → TLS gateway 8443. Настроить существующий
Ingress/Gateway/LB под выбранный домен: HTTPS upstream либо TLS passthrough,
WebSocket upgrade, таймаут запуска запроса не меньше 60 секунд, долгоживущий
WSS. Обычный HTTP upstream к gateway не поддержан: TLS в gateway обязателен.
Конкретный Ingress не генерируется, пока неизвестен контроллер кластера.

В пилоте браузер использует relay-only. Поддерживается IPv4 Pod-сеть. Worker слушает UDP 50000 на Pod IP;
Go control API — TCP 8081 внутри сети, без NodePort и без hostNetwork.
TURN должен маршрутизировать Pod CIDR всех узлов и достигать Pod IP:50000.
Worker должен достигать TURN listener и relay-портов. Если внешний TURN IP
находится за NAT, проверить hairpin-доступ из Pods; не считать его автоматическим.

На выбранном TURN-узле разрешить:

| Направление | Порты |
| --- | --- |
| Клиент → HTTPS/LB | TCP 443 |
| Клиент/worker → TURN | UDP/TCP 3478, TCP 5349 для TURN/TLS |
| Peer ↔ TURN relay | UDP 49160–49259 |
| TURN → worker Pod | UDP 50000 |
| Runner → worker Pod | TCP 8081 |

NetworkPolicy для hostNetwork зависит от CNI. Поэтому TURN-узел дополнительно
ограничивается firewall, а фактические source/destination после NAT проверяются
на узлах. Coturn разрешает peer IP из указанных worker Pod CIDR и TURN IP;
для общей Pod-сети это шире списка наших worker: нужен выделенный пул либо
сетевое ограничение на UDP 50000 только к нашим worker Pods.

HTTP worker API защищён индивидуальным Bearer-токеном и NetworkPolicy, но не TLS.
В пилоте предполагается доверенная сеть кластера; для недоверенной сети между
узлами требуется TLS/mTLS либо шифрование CNI.

## Приёмка на реальном кластере

1. Server-side dry-run, admission, загрузка образов и отсутствие ImagePullBackOff.
2. Готовность gateway/runner, SQLite на PVC; рестарт control Pod и восстановление
   доступа к пользователям. Активные приложения при рестарте завершаются.
3. Falkon с включённой sandbox, ввод, PDF-печать, отсутствие API-токена у приложения.
4. Разместить worker на разных узлах, проверить реальное relay-only видео/ввод/PDF
   с внешнего клиента. Отдельно TURN UDP, TCP, TLS.
5. Два пользователя × два приложения, отзыв прав, logout, завершение и cleanup;
   отказ узла, исчерпание ресурсов, таймаут pull, восстановление после рестарта.
6. Проверить запрет worker→worker и доступ только к согласованным приложениям.
7. Проверить резервную копию/восстановление SQLite согласованным snapshot/backup,
   не копированием только .db во время записи WAL.

Ни unit-тест Kubernetes API, ни Docker-тест не заменяют эту приёмку.
