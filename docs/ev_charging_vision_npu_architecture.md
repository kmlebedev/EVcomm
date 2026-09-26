# Архитектура видеоконтроля и автоматизации AC EV-зарядок на Radxa RK3588/NPU

## 1. Цель документа

Документ описывает обновлённую архитектуру системы управления простыми AC EV-постами без OCPP, в которой зарядный backend дополнительно использует видеонаблюдение и NPU платформы Radxa на RK3588 для распознавания автомобилей, парковочного места, состояния кабеля/коннектора и автомобильных номеров.

Основная идея: безопасность и включение силовой части остаются под контролем локального контроллера поста, а компьютерное зрение является дополнительным источником состояния и авторизации.

---

## 2. Базовые принципы архитектуры

1. **Не делать backend полноценным NVR**, если на объекте уже существует VMS/NVR.
2. **Не обрабатывать видео постоянно с полной частотой кадров** — использовать событийный анализ и ROI.
3. **Статические объекты не распознавать нейросетью каждый кадр**. Положение шкафа, портов и 4 парковочных мест задаётся при commissioning камеры.
4. **Использовать NPU RK3588 для inference**, CPU оставлять под backend, БД, API и оркестрацию.
5. **Go — основной production-язык**. C/C++ использовать только как тонкую прослойку к RKNN Runtime/OpenCV при необходимости.
6. **Нейросеть не должна напрямую управлять контактором**. Она формирует события и признаки для Authorization Engine.
7. **Результат компьютерного зрения подтверждать во времени**, а не принимать решение по одному кадру.
8. **Видео связывать с charging_session**, чтобы любая сессия имела доказательный временной диапазон.

---

# 3. Физическая схема одного узла

Типовой объект:

```text
                ┌──────────────────────┐
                │      IP Camera       │
                │  RTSP / ONVIF / VMS │
                └──────────┬───────────┘
                           │
                           │ Ethernet
                           ▼
                ┌──────────────────────┐
                │ Radxa / RK3588 node │
                │                      │
                │ Go Backend           │
                │ Media service        │
                │ Vision service       │
                │ RKNN Runtime         │
                │ PostgreSQL client    │
                └──────────┬───────────┘
                           │
                    LAN / RS-485 / IP
                           │
                           ▼
                ┌──────────────────────┐
                │ Local EV Controller │
                │ relay / contactor   │
                │ meter / protection  │
                └──────────┬───────────┘
                           │
                           ▼
                           EV
```

На одну камеру желательно проектировать **4 фиксированных парковочных места**, если геометрия позволяет уверенно видеть автомобили, кабели и номера.

---

# 4. Логическая архитектура

```text
RTSP / ONVIF / Existing VMS
            │
            ▼
      Media Gateway
      (MediaMTX)
            │
      ┌─────┴──────────────┐
      │                    │
      ▼                    ▼
Frame Sampler        Evidence Recorder
      │                    │
      ▼                    ▼
Vision Pipeline       Video references
      │                    │
      ▼                    │
RK3588 NPU                 │
      │                    │
      ▼                    │
Vision Events              │
      │                    │
      └──────────────┬─────┘
                     ▼
               Go Backend
                     │
      ┌──────────────┼──────────────┐
      ▼              ▼              ▼
Session Manager  Authorization   Incident/Event
                     Engine          Manager
      │              │              │
      └──────────────┼──────────────┘
                     ▼
               Port Controller
                     │
                     ▼
             Local Safety Logic
                     │
                     ▼
                  Contactor
```

---

# 5. Что распознаём нейросетью

Минимальный production-набор классов:

```text
vehicle
charging_cable
connector
license_plate
```

Опционально позже:

```text
person
smoke
open_cabinet_door
obstacle
```

## Что не нужно распознавать постоянно

Следующие объекты фиксируются один раз при конфигурации камеры:

```text
parking_zone_1 ... parking_zone_4
port_anchor_1 ... port_anchor_4
charger_zone
entry_zone
exit_zone
```

Они хранятся как полигоны или точки в конфигурации камеры.

---

# 6. Почему instance segmentation лучше простого detection

Bounding box достаточен для автомобиля и номера, но слаб для кабеля.

Для кабеля лучше получать маску:

```text
car mask
cable mask
connector bbox/mask
```

После inference backend дешёвой геометрией определяет:

```text
к какому порту относится кабель
к какому парковочному месту относится автомобиль
находится ли коннектор в зоне автомобиля
есть ли непрерывная связь port -> cable -> connector -> vehicle
```

Это позволяет не обучать отдельный сложный класс вроде `car_connected_to_port_3`.

---

# 7. Событийная модель компьютерного зрения

## 7.1 FAST pipeline

Работает постоянно или почти постоянно:

```text
1 FPS
640x384 / 640x360
маленькая INT8 модель
класс: vehicle
```

Назначение:

```text
PARKING_1 = FREE/OCCUPIED
PARKING_2 = FREE/OCCUPIED
PARKING_3 = FREE/OCCUPIED
PARKING_4 = FREE/OCCUPIED
```

## 7.2 DETAIL pipeline

Запускается только при событии:

```text
vehicle arrived
PORT_ENABLE requested
meter current changed
vehicle disappeared
connector state uncertain
```

Анализируется только ROI конкретного parking spot:

```text
vehicle
cable
connector
license_plate
```

Частота в момент события:

```text
3-5 FPS на 5-20 секунд
```

После стабилизации состояния:

```text
0.2-1 FPS
```

---

# 8. State Machine парковочного места

Для каждого места создаётся отдельная машина состояний.

```text
FREE
  ↓ vehicle detected
OCCUPIED_UNKNOWN
  ↓ plate recognized
OCCUPIED_IDENTIFIED
  ↓ cable connected
CONNECTED
  ↓ authorization success
AUTHORIZED
  ↓ current > threshold
CHARGING
  ↓ current == 0
CHARGING_STOPPED
  ↓ cable disconnected
DISCONNECTED
  ↓ vehicle left
FREE
```

Возможные аварийные состояния:

```text
PORT_ON_NO_VEHICLE
VEHICLE_PRESENT_NO_CABLE
CABLE_PRESENT_WRONG_SLOT
CONTACTOR_ON_NO_CURRENT
CURRENT_WITHOUT_AUTHORIZATION
VEHICLE_LEFT_WHILE_PORT_ACTIVE
VISION_UNCERTAIN
```

---

# 9. Связь Vision и физической телеметрии

Нельзя доверять только камере.

Итоговое состояние формируется из нескольких источников:

```text
Vision:
vehicle_present
plate
cable_visible
connector_at_vehicle

Controller:
relay_state
contactor_state
port_state

Meter:
voltage
current
power
energy

Backend:
authorization
session_state
user/account
```

Пример:

```text
vehicle_present = true
connector_at_vehicle = true
contactor = ON
current = 14.8A

=> CHARGING_CONFIRMED
```

Другой пример:

```text
vehicle_present = true
connector_at_vehicle = true
contactor = ON
current = 0A

=> EV_NOT_ACCEPTING_POWER
```

---

# 10. Архитектура NPU

Целевая платформа:

```text
Radxa board
SoC: RK3588 / RK3588S
NPU: Rockchip NPU
Runtime: RKNN Runtime
Model format: .rknn
Quantization: INT8
```

Pipeline подготовки модели:

```text
PyTorch training
      ↓
ONNX export
      ↓
RKNN Toolkit2
      ↓
calibration dataset
      ↓
INT8 quantization
      ↓
model.rknn
      ↓
RKNN Runtime on Radxa
```

В production **Python не нужен**.

---

# 11. Go + cgo архитектура

Предпочтительная граница:

```text
Go
 │
 │ cgo
 ▼
libevvision.so
 │
 ├── RKNN Runtime
 └── OpenCV / custom preprocessing
```

Минимальный C ABI:

```c
vision_handle* vision_create(const char* model_path);

int vision_infer(
    vision_handle* handle,
    const uint8_t* image,
    int width,
    int height,
    vision_result* result
);

void vision_destroy(vision_handle* handle);
```

Go-обёртка:

```go
type Engine interface {
    Infer(ctx context.Context, frame Frame) ([]Detection, error)
    Close() error
}
```

Production-код не должен знать детали RKNN.

---

# 12. Структура репозитория

Рекомендуемая структура:

```text
cmd/
  ev-backend/
  vision-bench/
  camera-probe/

internal/
  charging/
    session.go
    state_machine.go
    service.go

  devices/
    controller.go
    relay.go
    meter.go

  camera/
    camera.go
    onvif.go
    rtsp.go
    config.go

  media/
    mediamtx.go
    recorder.go
    evidence.go

  vision/
    engine.go
    rknn.go
    detector.go
    segmentation.go
    geometry.go
    tracker.go
    temporal.go

  parking/
    zone.go
    occupancy.go
    state_machine.go

  anpr/
    plate_detector.go
    ocr.go
    normalizer.go
    voting.go

  authorization/
    engine.go
    policies.go

  events/
    event.go
    bus.go

  storage/
    postgres/
    objectstore/

  api/
    http/
    grpc/

native/
  evvision/
    evvision.h
    evvision.cpp
    rknn_engine.cpp
    preprocess.cpp
    postprocess.cpp
    CMakeLists.txt

models/
  README.md
  manifests/

configs/
  cameras/
  sites/

deploy/
  systemd/
  docker/

scripts/
  export_onnx.py
  build_rknn.py
  benchmark.sh

docs/
```

---

# 13. Основные Go-интерфейсы

## Camera

```go
type Camera interface {
    ID() string
    Snapshot(ctx context.Context) ([]byte, error)
    StreamURL(ctx context.Context) (string, error)
}
```

## Vision Engine

```go
type VisionEngine interface {
    Infer(ctx context.Context, frame Frame) (VisionResult, error)
}
```

## Parking analyzer

```go
type ParkingAnalyzer interface {
    Analyze(result VisionResult, cfg CameraLayout) []ParkingObservation
}
```

## ANPR

```go
type ANPR interface {
    Recognize(ctx context.Context, vehicleROI Frame) (PlateResult, error)
}
```

## Authorization

```go
type AuthorizationEngine interface {
    Authorize(ctx context.Context, req AuthorizationRequest) (AuthorizationDecision, error)
}
```

## Port controller

```go
type PortController interface {
    Enable(ctx context.Context, portID string) error
    Disable(ctx context.Context, portID string) error
    State(ctx context.Context, portID string) (PortState, error)
}
```

---

# 14. Основные события

```text
vehicle.arrived
vehicle.departed
vehicle.identified
parking.occupied
parking.free
cable.detected
cable.connected
cable.disconnected
port.enable.requested
port.enabled
port.disabled
charging.started
charging.stopped
vision.uncertain
incident.created
```

Формат события:

```go
type Event struct {
    ID        string
    Type      string
    SiteID    string
    CameraID  string
    PortID    string
    SessionID string
    Timestamp time.Time
    Payload   json.RawMessage
}
```

---

# 15. Temporal voting

Нельзя менять состояние по единичной детекции.

Пример:

```text
frame 1: cable connected 0.82
frame 2: cable connected 0.91
frame 3: uncertain       0.55
frame 4: cable connected 0.94
frame 5: cable connected 0.90

=> CONNECTED
```

Рекомендуемая модель:

```text
sliding window = 5-10 observations
minimum confirmations = 3
state hysteresis = обязательна
```

Для ANPR:

```text
OCR(frame1)
OCR(frame2)
OCR(frame3)
...
        ↓
normalize
        ↓
majority / weighted voting
        ↓
stable plate
```

---

# 16. База данных

## charging_sessions

```text
id
site_id
port_id
account_id
vehicle_id
started_at
stopped_at
meter_start_wh
meter_stop_wh
status
```

## vehicles

```text
id
account_id
plate_normalized
plate_hmac
auto_charge_enabled
created_at
```

## camera_layouts

```text
camera_id
site_id
parking_zones JSONB
port_anchors JSONB
charger_zone JSONB
version
```

## vision_events

```text
id
camera_id
session_id
port_id
type
confidence
payload JSONB
ts
```

## evidence

```text
id
session_id
camera_id
video_start
video_end
video_ref
snapshot_ref
retention_until
incident_hold
```

---

# 17. Видеоархив

При наличии существующей VMS:

```text
Backend хранит только:

camera_id
session_id
start_time
end_time
VMS archive reference
snapshots
```

При отсутствии подходящего VMS API:

```text
MediaMTX
   ↓
fMP4 segments
   ↓
local NVMe
   ↓
retention 30 days
```

Рекомендуемый интервал evidence:

```text
session.start - 30 sec
session.stop  + 60 sec
```

При инциденте:

```text
incident_hold = true
```

и соответствующий материал исключается из обычной политики удаления.

---

# 18. Наблюдаемость

Prometheus metrics:

```text
vision_inference_seconds
vision_inference_total
vision_detection_total
vision_uncertain_total
vision_queue_depth
camera_stream_up
camera_frame_age_seconds
npu_utilization
cpu_usage
memory_usage
rtsp_reconnect_total
charging_sessions_active
```

Логи должны содержать:

```text
site_id
camera_id
port_id
session_id
trace_id
```

---

# 19. Roadmap: от стенда до production

## Phase 0 — лабораторный стенд

### Цель

Подтвердить, что камера физически видит:

```text
4 парковочных места
автомобиль
кабель
коннектор
номер
```

### Железо

```text
1 Radxa RK3588
1 IP camera
1 тестовый шкаф / макет
1-4 парковочных места
NVMe
```

### Написать

```text
camera-probe
  - RTSP connect
  - snapshot
  - latency/FPS statistics

vision-bench
  - load image/video
  - run RKNN inference
  - dump detections
  - measure latency
```

### Результат этапа

Получить реальный benchmark:

```text
model
resolution
FPS
NPU latency
CPU utilization
RAM
```

---

## Phase 1 — occupancy MVP

### Цель

Определять наличие автомобиля на каждом из 4 мест.

### Код

```text
internal/camera
internal/vision
internal/parking
```

### Реализовать

1. RTSP frame sampler.
2. INT8 vehicle detector на NPU.
3. Parking zones.
4. Intersection vehicle bbox/mask -> zone.
5. Temporal voting.
6. FREE/OCCUPIED state machine.
7. Prometheus metrics.

### Acceptance criteria

```text
устойчивое состояние FREE/OCCUPIED
нет дребезга состояния
работа день/ночь
автовосстановление RTSP
```

---

## Phase 2 — распознавание кабеля

### Цель

Определять связь:

```text
PORT_N -> CABLE -> VEHICLE_AT_SLOT_N
```

### Код

Добавить:

```text
vision/segmentation.go
vision/geometry.go
parking/connection.go
```

### Реализовать

1. Segmentation cable.
2. Detection/segmentation connector.
3. Port anchors.
4. Геометрию связи cable -> connector -> vehicle.
5. Temporal confirmation.

### Acceptance criteria

```text
CONNECTED
DISCONNECTED
UNCERTAIN
```

должны устойчиво определяться на типовых сценариях.

---

## Phase 3 — ANPR observational mode

### Цель

Распознавать номер, но пока не использовать для автоматического старта.

### Код

```text
internal/anpr/
```

### Реализовать

1. Vehicle ROI extraction.
2. Plate detection.
3. OCR.
4. Normalization.
5. Russian plate format validation.
6. Temporal voting.
7. Confidence statistics.

### Хранить

```text
recognized_plate
confidence
confirmed_plate
camera_id
time_of_day
snapshot_ref
```

### Результат

Накопить реальный dataset ошибок.

---

## Phase 4 — charging session + evidence

### Цель

Связать физическую зарядку и видео.

### Код

```text
internal/charging
internal/media
internal/events
internal/storage
```

### Реализовать

```text
charging_session
camera association
video time range
snapshots
meter timeline
vision timeline
port timeline
```

### Результат

Любая зарядка имеет единую временную шкалу событий.

---

## Phase 5 — assisted authorization

### Цель

Использовать распознанный номер как дополнительный credential.

Сценарий:

```text
vehicle appears
    ↓
plate recognized
    ↓
known account
    ↓
backend sends confirmation
    ↓
user confirms
    ↓
port authorization
```

### Реализовать

```text
vehicles table
authorization policies
user confirmation
security audit
```

На этом этапе ANPR ещё не включает контакт напрямую.

---

## Phase 6 — AutoCharge

### Условия включения

AutoCharge разрешается только если:

```text
vehicle is known
plate recognition stable
confidence above calibrated threshold
correct parking slot
correct cable/connector relation
account active
auto_charge enabled
port available
local controller healthy
```

### Последовательность

```text
Vision
  ↓
Authorization Engine
  ↓
Port Controller
  ↓
Local Safety Controller
  ↓
Contactor
```

Нельзя делать:

```text
Vision -> Contactor
```

---

## Phase 7 — pilot deployment

### Масштаб

```text
1 объект
4-16 парковочных мест
несколько камер
```

### Проверять

```text
RTSP stability
NPU temperature
NPU utilization
false positive/negative
night quality
rain/snow
headlights
camera vibration
dirty plates
partial occlusion
cable visibility
```

### Добавить

```text
remote diagnostics
health endpoint
model version reporting
camera layout version
remote config rollout
```

---

## Phase 8 — production

### Требования

1. Versioned camera configurations.
2. Versioned models.
3. Safe OTA deployment.
4. Rollback.
5. Health monitoring.
6. Watchdog.
7. Automatic restart.
8. Database backup.
9. Evidence retention.
10. Incident hold.
11. Audit log.

---

# 20. Model lifecycle

Production-модель должна иметь manifest:

```yaml
name: ev-parking-seg
version: 1.3.0
input: 640x640
format: rknn
quantization: int8
classes:
  - vehicle
  - cable
  - connector
sha256: ...
```

Backend при запуске пишет:

```text
model_version
model_hash
rknn_runtime_version
hardware_revision
```

Это необходимо для расследования ошибок.

---

# 21. Benchmark suite

Обязательно создать отдельную утилиту:

```text
cmd/vision-bench
```

Параметры:

```text
-model
-video
-resolution
-fps
-duration
```

Вывод:

```text
frames_processed
avg_inference_ms
p50
p95
p99
NPU utilization
CPU utilization
RAM
thermal
```

Benchmark должен стать частью acceptance criteria каждой новой модели.

---

# 22. Dataset

С самого первого пилота сохранять hard examples:

```text
false positive
false negative
uncertain
night
rain
snow
headlights
dirty car
occluded cable
wrong parking
```

Структура:

```text
dataset/
  images/
  labels/
  metadata/
```

Metadata:

```text
camera_id
site_id
time
lighting
weather
model_version
prediction
confirmed_result
```

Обучение делается отдельно от production backend.

---

# 23. Что писать первым в коде

Рекомендуемый фактический порядок разработки:

```text
1. camera-probe
2. vision-bench
3. RKNN cgo wrapper
4. vehicle detector
5. parking zone geometry
6. temporal voting
7. occupancy state machine
8. event bus
9. charging session integration
10. cable segmentation
11. connector relation logic
12. ANPR
13. evidence/video integration
14. authorization engine
15. assisted authorization
16. AutoCharge
17. fleet monitoring / OTA
```

Не начинать разработку с AutoCharge или сложного UI.

Сначала нужно доказать качество зрения и стабильность камеры.

---

# 24. Минимальный MVP

Первый полезный production MVP может вообще не включать AutoCharge.

Он должен уметь:

```text
RTSP camera
↓
vehicle occupancy
↓
charging session
↓
video evidence
↓
physical meter/relay telemetry
↓
incident timeline
```

Это уже даёт существенную ценность для эксплуатации и разбирательств.

Следующим релизом:

```text
cable state
ANPR
```

И только затем:

```text
plate-assisted authorization
AutoCharge
```

---

# 25. Итоговая целевая архитектура

```text
                           Existing CCTV/VMS
                                  │
                              RTSP/ONVIF
                                  │
                                  ▼
                             MediaMTX
                                  │
                 ┌────────────────┴───────────────┐
                 │                                │
                 ▼                                ▼
          FAST frame sampler                Video evidence
                 │                                │
                 ▼                                │
            Vehicle INT8                          │
                 │                                │
                 ▼                                │
            RK3588 NPU                            │
                 │                                │
                 ▼                                │
          Parking occupancy                       │
                 │                                │
                 ├── event ──────┐                │
                 │               ▼                │
                 │        DETAIL ROI pipeline     │
                 │               │                │
                 │        ┌──────┼──────┐         │
                 │        ▼      ▼      ▼         │
                 │      cable connector ANPR      │
                 │        │      │      │         │
                 └────────┴──────┴──────┴─────────┘
                                  │
                                  ▼
                             Event Engine
                                  │
                                  ▼
                           Charging Session
                                  │
                ┌─────────────────┼──────────────────┐
                ▼                 ▼                  ▼
             Vision           Meter data         Evidence
                │                 │                  │
                └─────────────────┼──────────────────┘
                                  ▼
                         Authorization Engine
                                  │
                                  ▼
                           Port Controller
                                  │
                                  ▼
                       Local Safety Controller
                                  │
                                  ▼
                               Contactor
                                  │
                                  ▼
                                  EV
```

---

# 26. Ключевое архитектурное решение

Компьютерное зрение в этой системе является не отдельной функцией видеонаблюдения, а **ещё одним сенсором зарядного backend**.

Его задача — дополнять электрические и логические данные визуальными фактами:

```text
кто приехал
куда припарковался
подключён ли кабель
к какому порту он подключён
какой автомобиль заряжается
когда автомобиль уехал
что визуально происходило во время сессии
```

При этом физическая безопасность, электрические защиты и конечное разрешение силовой коммутации должны оставаться в локальном контроллере зарядного поста.
