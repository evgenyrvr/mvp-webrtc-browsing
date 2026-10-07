# mvp1 — C4-архитектура

Дата: 2026-09-17. Целевая архитектура принятых ADR, с отдельным указанием
границ первого инкремента. Диаграммы описаны в Mermaid. C4 container означает
исполняемую часть системы или хранилище, не обязательно Docker-контейнер.
Модель: [официальное описание C4](https://c4model.com/diagrams).

## L1 — System Context

```mermaid
flowchart LR
    U[Пользователь\nLinux / Windows / macOS] -->|Работает с опубликованными приложениями| P
    A[Администратор] -->|Назначает роли и приложения| P
    O[Оператор Linux-стенда] -->|Развёртывает, задаёт шаблоны и сеть| P
    P[mvp1\nПубликация приложений через WebRTC] -->|Обращения серверного приложения| N[Разрешённые сервисы защищённой сети]
    P -->|PDF через клиент| Q[Локальная система печати пользователя]
```

Два независимых пользователя, по два активных приложения, максимум четыре
сеанса. Пользователь видит только разрешённые публикации. Его компьютер
не получает универсальный сетевой туннель. Администратор управляет доступом,
оператор задаёт доверенные шаблоны приложений и сетевую политику.

## L2 — Containers

```mermaid
flowchart TB
    U[Пользователь / администратор] --> UI
    subgraph Client[Компьютер пользователя]
        UI[Web UI\nChrome-совместимый браузер\nHTML / CSS / JavaScript]
    end
    subgraph Linux[Linux-сервер — mvp1]
        G[Go gateway\nHTTP API, UI, RBAC, сигналинг]
        D[(SQLite\nПользователи, роли, сеансы)]
        R[Go runner\nЛокальное управление runtime]
        subgraph Session[Окружение приложения — до 4 экземпляров]
            W[Go worker / Pion\nWebRTC endpoint]
            APP[Falkon / Firefox / terminal\nXvfb / FFmpeg / виртуальная печать]
        end
    end
    T[coturn\nОтдельный TURN/STUN сервис]
    N[Сервисы защищённого сегмента]
    UI <-->|HTTPS / WSS| G
    G -->|SQL, локальный файл| D
    G <-->|Приватный Unix socket| R
    R -->|Создание / завершение| Session
    G <-->|Приватный аутентифицированный сигналинг| W
    W <-->|Кадры, XTEST, PDF spool| APP
    UI <-->|WebRTC напрямую, если разрешено| W
    UI <-->|WebRTC relay| T
    T <-->|WebRTC relay| W
    APP -->|Разрешённые назначения| N
```

| Container | Обязанности и граница доверия |
| --- | --- |
| Web UI | Вход, каталог, вкладки приложений, состояния подключения, ввод, локальная печать; не источник прав |
| Go gateway | Публичная точка входа, серверная авторизация каждого действия, выдача TURN credentials |
| SQLite | Локальная БД gateway; недоступна пользовательским окружениям |
| Go runner | Только предустановленные шаблоны; доступ к runtime не передаётся клиентам и приложениям |
| Go worker | Один сеанс, одно поколение управляющего соединения; без доступа к БД пользователей |
| Окружение | Отдельные профиль, display, spool, лимиты ресурсов и egress |
| coturn | Relay для видео и DataChannels; не SFU и не HTTP API gateway |

Falkon, Firefox, Xvfb и FFmpeg входят в worker image; виртуальный IPP-принтер реализован на Go.
Собственная серверная логика пишется на Go. Первое окружение testing и рабочее
deploy используют раздельные БД, профили, секреты и артефакты.

## L3 — Components

### Go gateway

```mermaid
flowchart LR
    H[HTTP / WSS handlers\n/api/v1] --> AUTH[Auth service]
    H --> ACCESS[Access service / RBAC]
    H --> CAT[Application catalog]
    H --> S[Session service\nЛимиты, ownership, lifecycle]
    H --> SIG[Signaling broker]
    AUTH --> DB[SQLite repository]
    ACCESS --> DB
    CAT --> DB
    S --> ACCESS
    S --> DB
    S --> RT[Runner client]
    SIG --> S
    SIG --> WT[Worker transport]
```

Объединение ролей: OR разрешений с обязательным итоговым `can_launch`.
Login не занимает слот приложения. Активный пользователь — владелец хотя бы
одного незавершённого app_session. Любое незавершённое состояние, включая
preparing, ready, connecting, reconnecting и stopping, занимает слот до cleanup.
Проверка лимитов и резервирование выполняются атомарно.

### Go session worker

```mermaid
flowchart LR
    C[Session controller\nВладелец, поколение, права] --> RTC[Pion PeerConnection]
    CAP[Capture adapter\nFFmpeg / VP8] -->|Кодированные кадры| RTC
    RTC -->|control / pointer| INPUT[Input adapter\nВалидация / XTEST]
    PRINT[Print bridge\nГотовый PDF, лимиты, checksum] -->|print DataChannel| RTC
    C --> LIFE[Process lifecycle\nStart, cancel, cleanup]
    LIFE --> CAP
    C --> INPUT
    C --> PRINT
```

Старое поколение не вводит команды и не завершает новое соединение.
Права проверяются на сервере. Пропажа управляющего канала блокирует ввод и печать.
Отзыв launch закрывает приложение; logout/отключение пользователя — все его сеансы.

## L4 — Code / границы модулей

Эскиз интерфейсов, а не обещание уже существующих типов; реализация уточняется
при разработке. Основные зависимости направлены от HTTP к сервисам и адаптерам.

```mermaid
classDiagram
    class HTTPServer {
        ServeHTTP()
    }
    class Store {
        Authenticate()
        Permissions()
        ReserveSession()
        ListSessions()
    }
    class SessionService {
        Launch()
        Stop()
        StopUser()
    }
    class Runtime {
        <<interface>>
        Start()
        Stop()
    }
    class RunnerClient
    class Worker {
        Negotiate()
        HandleInput()
        DeliverPDF()
        Close()
    }
    HTTPServer --> Store
    HTTPServer --> SessionService
    SessionService --> Store
    SessionService --> Runtime
    RunnerClient ..|> Runtime
    RunnerClient --> Worker : управляет через runner
```

Go-пакеты: `internal/store`, `internal/sessions`, `internal/server`, затем
`internal/runtime`, `internal/rtc`, `internal/input`, `internal/printing`.
`web` содержит встраиваемый UI. Unit-тесты — возле пакетов, отдельные сценарии
и тестовый запуск — в `testing/`; рабочий запуск — в `deploy/`.

## Динамика клиентского пути

```mermaid
sequenceDiagram
    actor U as Пользователь
    participant B as Браузер
    participant G as Gateway
    participant D as SQLite
    participant R as Runner
    participant W as Worker приложения
    U->>B: Вход и выбор Firefox
    B->>G: POST login, GET applications
    G->>D: Учётная запись и роли
    G-->>B: Каталог разрешённых приложений
    B->>G: POST sessions(application_id)
    G->>D: Атомарно проверить права и 2/2/4, зарезервировать
    G->>R: Запустить утверждённый шаблон
    R->>W: Создать изолированное окружение
    W-->>G: Готовность
    G-->>B: Сеанс готов
    B->>G: SDP offer / ICE
    G->>W: Сигналинг
    W-->>B: SDP answer / ICE через gateway
    W-->>B: Видео по WebRTC, при необходимости через TURN
    B->>W: Ввод по DataChannel
    W-->>B: PDF по DataChannel
    U->>B: Открыть документ и печатать локально
```

Terminal во второй вкладке повторяет создание независимого сеанса. Закрытие
одного приложения не влияет на другое. В будущем лимиты могут стать 2/3/6
после нагрузочной проверки.

## Эволюция и состояние реализации

- Будущий API gateway располагается перед Go backend и маршрутизирует HTTP/WSS.
  Предметная авторизация остаётся в backend; медиа не направляются через HTTP gateway.
- Возможный Electron-клиент использует те же API/WebRTC и отдельные адаптеры ОС.
- Первый инкремент: C4, SQLite/auth/RBAC/catalog, атомарные лимиты сеансов,
  браузерный UI и тесты. Второй инкремент добавляет Docker runner, Pion worker,
  Xvfb/FFmpeg, XTEST и браузерный плеер. PDF пока не реализован.
  Изоляция процессов уточнена в [ADR-006](adr/006-worker-process-isolation.md).
- Terminal с изолированной shell, одноразовые профили и PDF с локальным диалогом
  подтверждены в [ADR-005](adr/005-first-pilot-behavior.md). Перед сетевым запуском
  уточняются TURN и разрешённые сетевые назначения. Состояние готовности
  фиксируется в README проекта.

Основания: [архитектура](architecture.md), [ADR-001](adr/001-remote-app-sessions.md),
[ADR-002](adr/002-concurrent-applications.md), [ADR-003](adr/003-api-gateway-evolution.md),
[ADR-004](adr/004-client-platforms-and-mvp-experience.md).

### Реализованная печать: уточнение компонента worker

Внутри каждого worker компонент Go IPP слушает loopback `127.0.0.1:8631`.
Firefox через клиентскую библиотеку отправляет PDF этому компоненту; отдельного
демона CUPS нет. Go IPP проверяет право печати и передаёт документ в ограниченную
очередь памяти worker. Отправитель DataChannel `print` доставляет PDF только
текущему клиенту сеанса. Web UI проверяет SHA-256 и открывает PDF по действию
пользователя; локальная ОС отвечает за диалог и физическую печать.
Подробности и ограничения: [ADR-007](adr/007-go-ipp-printing.md).

## Размещение в многоузловом Kubernetes (2026-10-07)

См. [ADR-009](adr/009-kubernetes-deployment.md) и [инструкцию запуска](../deploy/kubernetes/README.md).

```mermaid
flowchart LR
    C[Клиент] -->|HTTPS / WSS| LB[Ingress / LB оператора]
    subgraph K[Кластер Kubernetes на Linux]
        subgraph CP[Control Pod — 1 реплика]
            G[Go gateway] <-->|Unix socket| R[Go runner]
        end
        G --> DB[(SQLite PVC / block CSI)]
        R -->|HTTPS / ServiceAccount| API[Kubernetes API]
        API -->|Создание / удаление| W[Session Pods на разных узлах
Go worker + Falkon / Firefox / terminal]
        R -->|HTTP / индивидуальный токен| W
        T[coturn / hostNetwork
Закреплённый узел] <-->|UDP / маршруты Pod CIDR| W
    end
    LB --> G
    C <-->|WebRTC relay-only| T
```

Gateway и runner размещаются вместе, чтобы сохранить приватный Unix socket.
Много узлов позволяют распределять приложения, но не означают несколько реплик
управляющего сервиса: SQLite и состояние сеансов требуют одного writer.
Приложения не получают ServiceAccount token. Перезапуск runner очищает его старые
Pods; продолжение прежних интерактивных сеансов после рестарта не обещается.
