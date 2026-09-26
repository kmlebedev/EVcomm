# EVcomm: развитие архитектуры щита с Type 2 / EKEPC2-S

Версия 0.1 · 26 сентября 2026

Статус: архитектурное решение для стенда. До серийного применения обязательны стендовые испытания конкретной ревизии EKEPC2-S, проверка электрической схемы и защит специалистом по силовой части.

## 1. Цель изменения

Добавить к существующей архитектуре EVcomm управляемый порт IEC 62196-2 Type 2 Mode 3 без покупки полноценного wallbox/OCPP-контроллера:

- отдельная панельная розетка Type 2 32 A с CP, PP и электромеханическим замком;
- отдельный локальный EVSE-контроллер ETEK EKEPC2-S;
- существующий WB-MR6C сохраняется как исполнитель разрешения EVcomm и watchdog;
- существующий силовой контактор Kx сохраняется;
- силовая цепь включается только при одновременном разрешении EVcomm и разрешении EKEPC2;
- EKEPC2 отвечает за IEC 61851 CP/PP, lock, RCMU и локальную EVSE state machine;
- Go/EVcomm отвечает за доступ пользователя, сессию, энергетический бюджет, журнал, диагностику и позднее — динамический лимит тока через CP PWM.

Базовый принцип:

```text
Kx_COIL = EVCOMM_GRANT && EKEPC2_CONTACTOR_PERMIT
```

То есть ни Go/MR6C, ни EKEPC2 по отдельности не должны иметь возможность подать силовое питание на Type 2.

## 2. Изменение схемы щита

### 2.1. Было

```text
Raspberry Pi / Go
       |
     Ethernet
       |
   WB-MGE v.3
       |
 RS485-1 / Modbus TCP :502
       |
   WB-MR6C
       |
   relay Kx command
       |
   contactor Kx
       |
   meter
       |
   EVSE / outlet
```

### 2.2. Становится для Type 2

```text
                         Raspberry Pi / EVcomm
                                  |
                               Ethernet
                                  |
                              WB-MGE v.3
                                  |
                 RS485-1 / Modbus TCP :502
                     9600 8N1, one bus owner
                         /                 \
                  WB-MR6C             EKEPC2-S
                slave=N                 slave=M
                    |                     |
        EVcomm permit relay         IEC 61851 state
                    |                     |
                    +------ SERIES -------+
                              |
                         Kx coil / Kx
                              |
                    certified line meter
                              |
                   Type 2 L/N/(L2/L3)/PE

Type 2 CP --------------------------> EKEPC2-S CP
Type 2 PP --------------------------> EKEPC2-S PP
Type 2 lock motor <-----------------> EKEPC2-S MH/ML
Type 2 lock feedback ---------------> EKEPC2-S lock feedback
RCMU -------------------------------> EKEPC2-S RCMU inputs/test
Kx auxiliary NC --------------------> WB-MR6C input
```

Силовой контактор не дублируется. Контакт разрешения MR6C и пассивный выход управления контактором EKEPC2 включаются последовательно в цепь катушки Kx.

## 3. Разделение ответственности

### EKEPC2-S

Локально и независимо от Linux/backend:

- состояние CP A/B/C/D/E/F;
- PWM IEC 61851;
- PP и допустимый ток кабеля;
- блокировка/разблокировка Type 2;
- RCMU self-test/fault;
- выдача контакторного permit;
- локальное прекращение permit при ошибке EVSE.

### WB-MR6C

- физический EVcomm permit для каждого порта;
- обратная связь вспомогательного контакта Kx;
- безопасное состояние OFF;
- watchdog по прекращению корректного Modbus-опроса MR6C.

### EVcomm / Go

- грант пользователя;
- state machine сессии;
- бюджет мощности;
- координация MR6C + EKEPC2;
- чтение состояния EKEPC2;
- установка разрешённого тока/PWM;
- проверка фактического состояния Kx;
- метрология и снимки энергии;
- журнал событий и observability.

## 4. Критическая проверка совместимости RS-485

### 4.1. WB-MGE v.3

Wiren Board документирует для каждого RS-485 порта режим `Modbus TCP`: WB-MGE принимает стандартный Modbus TCP, переносит Unit ID в Modbus RTU slave address и передаёт запрос на общую RS-485 шину. На один порт допускается несколько Modbus RTU устройств с одинаковыми UART-параметрами. По умолчанию RS485-1 опубликован на TCP 502, RS485-2 — 503.

Источник:
- https://wiki.wirenboard.com/wiki/WB-MGE_v.3_Modbus_Ethernet_Gateway
- https://wirenboard.com/ru/contents/product/wb-mge-v3/

### 4.2. UART-параметры

Текущий EVcomm roadmap использует MR6C на RS485-1 с `115200`.

Свежая инструкция ETEK от 14.04.2026 для EKEPC2-C/S указывает:

```text
Modbus RTU
9600 bit/s
8 data bits
No parity
1 stop bit
address 1..255
factory/default shown as 255
```

Источник:
- https://www.etek-electric.com/kb-articles/how-to-setting-rs485-communication-for-ekepc2-c-s-epc-controller

Старая документация ETEK указывает 38400 8N1. Это означает, что версия firmware/hardware должна быть зафиксирована в BOM, а фактические UART-параметры проверены на стенде до закупки партии.

WB-MR6C поддерживает 9600 8N2 по умолчанию. Wiren Board отдельно указывает, что актуальные прошивки MR6C работают при несовпадении числа stop bits с master; следовательно, настройка WB-MGE `9600 8N1` совместима с EKEPC2 и должна быть проверена с конкретным MR6C v.3.

Источник:
- https://wiki.wirenboard.com/wiki/WB-MR6C_v.3_Modbus_Relay_Modules

### 4.3. Решение для одной шины

Для стенда принять:

```text
WB-MGE RS485-1:
  mode: Modbus TCP
  tcp_port: 502
  baud: 9600
  data_bits: 8
  parity: N
  stop_bits: 1
  terminator: only if WB-MGE is physical end of line
  failsafe_bias: enable only at the master side as recommended by Wiren Board

MR6C:
  address: 10 (пример)
  baud: 9600
  parity: N
  own stop-bit behavior verified on bench

EKEPC2-S #1:
  address: 20
  baud: 9600
  8N1

EKEPC2-S #2 later:
  address: 21
  baud: 9600
  8N1
```

Не оставлять EKEPC2 на заводском `255`: задать уникальный адрес до включения нескольких контроллеров на общую шину.

### 4.4. EKEPC2 не должен становиться вторым master

Документация EKEPC2 содержит режимы, в которых контроллер сам опрашивает внешний счётчик/DLB meter по Modbus RTU. На общей шине EVcomm это создаёт риск multi-master collision.

Для первой версии на общей RS485-1:

- DLB EKEPC2 выключить;
- внешний meter polling EKEPC2 не использовать;
- регистры адресов внешних meter 90..96 оставить disabled (`65535` там, где это предусмотрено документацией);
- динамическое распределение мощности делать из EVcomm, изменяя разрешённый PWM через register 109;
- EKEPC2 использовать только как Modbus slave.

Это условие является обязательным стендовым тестом: анализатор RS-485 должен подтвердить, что EKEPC2 самостоятельно не передаёт кадры в шину при выбранной конфигурации.

## 5. Проверка текущей реализации Modbus EVcomm

Текущий публичный roadmap/README EVcomm подтверждает:

- `internal/hardware/mr6c` использует `simonvetter/modbus` и Modbus TCP к `WB-MGE:502`;
- на пост предполагается одна goroutine и одно TCP-соединение;
- MR6C сейчас опрашивается примерно раз в 100 ms;
- `RS485-1` сейчас планируется на 115200;
- `RS485-2` используется независимо для Mercury через transparent bridge :503.

Источники:
- https://github.com/kmlebedev/EVcomm
- https://github.com/kmlebedev/EVcomm/blob/main/docs/roadmap_evse_line_control_go_2026-09-25.md

### 5.1. Что можно оставить

- стандартный Modbus TCP transport `simonvetter/modbus`;
- одно TCP соединение на пост/RS485-1;
- reconnect/backoff;
- lease/watchdog архитектуру;
- MR6C driver как device-level adapter.

### 5.2. Что нужно изменить

Сейчас соединение концептуально принадлежит MR6C. После появления второго slave оно должно принадлежать **RS-485 bus**, а device drivers должны отправлять транзакции через общий bus executor.

Причина: в `simonvetter/modbus` `SetUnitId()` имеет mutex, и Modbus request также имеет mutex, но две операции не образуют единую атомарную транзакцию. Такой код опасен:

```go
client.SetUnitId(mr6cID)
client.ReadCoils(...)
```

если параллельно другая goroutine выполняет:

```go
client.SetUnitId(ekepcID)
client.ReadRegisters(...)
```

Между `SetUnitId()` и `Read...()` второй caller может сменить Unit ID.

Источник реализации библиотеки:
- https://github.com/simonvetter/modbus/blob/master/client.go

### 5.3. Новый bus abstraction

Добавить:

```text
internal/modbusbus/
    bus.go
    request.go
    scheduler.go
    metrics.go
    reconnect.go
```

Минимальный интерфейс:

```go
type Bus interface {
    Do(ctx context.Context, unitID uint8, fn func(Client) error) error
}
```

`Do()` обязан держать один bus mutex на всей последовательности:

```text
lock
  SetUnitId(unitID)
  request
  wait response / timeout
unlock
```

Ещё лучше не передавать raw client наружу, а дать typed operations:

```go
ReadCoils(ctx, unit, addr, qty)
ReadDiscreteInputs(ctx, unit, addr, qty)
ReadHolding(ctx, unit, addr, qty)
WriteSingleCoil(ctx, unit, addr, value)
WriteSingleRegister(ctx, unit, addr, value)
```

Так невозможно случайно выполнить запрос вне общей очереди.

## 6. Новый драйвер EKEPC2

Добавить:

```text
internal/hardware/ekepc2/
    device.go
    registers.go
    state.go
    config.go
    verify.go
    current.go
    errors.go

cmd/ekepc2-bench/
    main.go

internal/sim/ekepc2/
    server.go
    state_machine.go
```

### 6.1. Минимальные регистры первой версии

По актуальной документации ETEK:

```text
89   remote start/stop                 R/W   (не делать safety-critical до проверки семантики)
100  device address                    R/W
109  max output PWM duty *100          R/W
110  RCMU function                     R/W
112  lock function                     R/W
114  DLB function                      R/W
127  pole selection                    R/W
140  software version                  R
141  current working state             R
142  cable/PP PWM                      R
143  RCMU status                       R
145  lock status                       R
146  DLB current                       R
151  rotary-switch PWM                 R
152  actual output PWM                 R
157  temperature                       R
```

Для 32 A по формуле ETEK:

```text
PWM register 109 = 32 / 0.6 * 100 = 5333
```

Для 16 A:

```text
16 / 0.6 * 100 = 2667 (округление/точное допустимое значение подтвердить на стенде)
```

Источник:
- https://www.etek-electric.com/1369-2
- https://www.etek-electric.com/kb-articles/how-to-setting-rs485-communication-for-ekepc2-c-s-epc-controller

### 6.2. Не доверять неподтверждённым полям

В документации ETEK регистры 147..150, 153..156 помечены как temporarily invalid. Не использовать их для безопасности, учёта или подтверждения наличия напряжения.

## 7. Доменная модель EVcomm

Добавить к линии отдельную сущность EVSE, не смешивая её с `LineState` контактора.

Пример:

```go
type EVSEState string

const (
    EVSEUnknown   EVSEState = "UNKNOWN"
    EVSEAvailable EVSEState = "AVAILABLE" // CP A
    EVSEConnected EVSEState = "CONNECTED" // CP B
    EVSECharging  EVSEState = "CHARGING"  // CP C
    EVSEVentReq   EVSEState = "VENT_REQ"  // CP D
    EVSEFault     EVSEState = "FAULT"
)

type EVSEObservation struct {
    State          EVSEState
    WorkingState   uint16
    PWM            uint16
    CablePWM       uint16
    RCMUStatus     uint16
    LockStatus     uint16
    TemperatureRaw uint16
    Firmware       uint16
    ObservedAt     time.Time
    Quality        Quality
}
```

Линия и EVSE остаются независимы:

```text
LineState = подтверждено положение силового контактора
EVSEState = состояние CP/RCMU/lock контроллера
SessionState = бизнес-сессия пользователя
```

Нельзя выводить одно из другого.

## 8. Контроллер поста: новый порядок включения

Первый безопасный вариант:

1. пользователь получает grant;
2. проверить lease и бюджет;
3. EKEPC2 online, firmware/config verified;
4. RCMU status healthy;
5. lock state допустим;
6. CP state допустим для начала сессии;
7. установить current limit/PWM;
8. проверить чтением register 152;
9. выдать EVcomm permit через MR6C;
10. EKEPC2 самостоятельно замыкает свой контакторный permit только при корректном CP state;
11. подтвердить Kx по MR6C output + real state + auxiliary NC;
12. подтвердить переход EVSE в charging state;
13. сделать энергетический snapshot и перевести session в charging/active.

OFF:

1. снять EVcomm permit MR6C;
2. подтвердить отпускание Kx;
3. убедиться, что EVSE больше не сообщает charging;
4. снять финальный energy snapshot;
5. закрыть session.

Emergency/fault:

- RCMU fault / EVSE fault => Kx должен отпасть локально через EKEPC2 permit;
- EVcomm затем должен увидеть несоответствие/FAULT и снять собственный MR6C permit;
- автоматического повторного ON после fault в первом production release не делать.

## 9. Watchdog MR6C и общая шина

Wiren Board пишет, что таймер безопасного режима MR6C перезапускается после каждого успешно обработанного Modbus-пакета.

На общей шине необходимо экспериментально подтвердить важное свойство:

> кадр, адресованный EKEPC2, НЕ должен считаться MR6C успешно обработанным пакетом и не должен продлевать Safety Poll Timeout MR6C.

Это ожидаемое поведение корректного Modbus slave (чужой address игнорируется), но для safety case оно должно быть не предположением, а результатом теста конкретной прошивки.

Тест:

1. MR6C K1 ON, safety timeout = 3 s;
2. прекратить запросы к slave MR6C;
3. продолжать каждые 100 ms опрашивать только EKEPC2;
4. анализатором подтвердить трафик;
5. MR6C обязан уйти в safe OFF примерно через заданный timeout;
6. повторить минимум 100 раз и после power cycle.

Если тест не проходит — MR6C и EKEPC2 нельзя оставлять на одной RS-485 шине при текущей watchdog-модели; потребуется отдельный физический RS-485 канал/шлюз или другой аппаратный watchdog.

Источник watchdog:
- https://wiki.wirenboard.com/wiki/WB-MR6C_v.3_Modbus_Relay_Modules

## 10. Бюджет общей шины 9600 bit/s

Переход MR6C с 115200 на 9600 снижает пропускную способность в 12 раз. Текущий цикл EVcomm ~100 ms надо повторно измерить.

Не принимать расчёт только по номинальной скорости. На стенде измерить p50/p95/p99 для полного цикла:

```text
MR6C:
  FC02 inputs
  FC02 real states
  FC01 outputs

EKEPC2:
  FC03 status block 140..157 (разбить, если контроллер не принимает длинное чтение)

+ command/write traffic
```

Начальная стратегия scheduler:

```text
priority 0: OFF / emergency / safety verification
priority 1: command confirmation
priority 2: MR6C watchdog/status poll
priority 3: EKEPC2 state poll
priority 4: slow diagnostics/config
```

Предварительные периоды для стенда:

```text
MR6C critical poll: 100..200 ms
EKEPC2 state:       200..500 ms
EKEPC2 temperature: 5 s
configuration:      on connect + rare verification
```

Утверждать периоды только после измерения загрузки и p99.

## 11. Изменение registry/config

Пример:

```yaml
posts:
  - id: bench01
    modbus_bus:
      gateway: 192.168.1.50:502
      baud: 9600
      data_bits: 8
      parity: N
      stop_bits: 1
      timeout: 500ms

    mr6c:
      address: 10
      firmware_min: "1.24.0"

    lines:
      - id: L1
        relay_channel: 1
        contactor_feedback_input: 1
        meter: meter_l1
        evse:
          type: ekepc2
          address: 20
          expected_firmware: 1002
          socket: true
          phases: 1
          max_current_a: 32
          rcmu_required: true
          lock_required: true
          dlb_internal: false
```

Validation должен запрещать:

- duplicate slave addresses на одном bus;
- address 0;
- EKEPC2 address 255 в production config;
- разные UART настройки устройств одного bus;
- Type 2 socket без lock, если `lock_required`;
- Type 2 production port без RCMU, если policy требует его;
- EKEPC2 DLB/master mode на общей EVcomm bus.

## 12. Симулятор

Расширить `simonvetter/modbus` server simulator так, чтобы один Modbus TCP endpoint поддерживал несколько Unit ID:

```text
unit 10 -> MR6C simulator
unit 20 -> EKEPC2 simulator
unit 21 -> optional second EKEPC2
```

`simonvetter/modbus` server передаёт `UnitId` в request handler, поэтому такой multi-slave simulator естественно реализуется одним TCP сервером.

Источник:
- https://github.com/simonvetter/modbus/blob/master/server.go

EKEPC2 simulator должен уметь:

- A/B/C/D/fault transitions;
- lock OK/fault;
- RCMU OK/self-test fail/leak;
- delayed contactor permit;
- ignore write;
- malformed/exception response;
- timeout;
- firmware mismatch;
- PWM readback mismatch;
- spontaneous reset to defaults.

## 13. Bench CLI

Добавить `cmd/ekepc2-bench`:

```text
identity             read firmware/address
status               read 140..157
verify               compare expected config
set-address 20       explicit protected operation
set-current 16       set register 109 and verify 152
set-current 32
lock-status
rcmu-status
watch                 continuously print CP/lock/RCMU/PWM
latency -n 10000
faults                 assisted fault checklist
```

Изменение адреса и protected config — только с явным `--apply` + confirmation на bench; line-controller в normal mode не должен переписывать device address.

## 14. Roadmap: стенд -> production

### Этап T0 — заморозить аппаратную ревизию

Результат:

- купить 2–3 одинаковых EKEPC2-S;
- записать маркировку PCB, firmware register 140, дату партии;
- получить Type 2 socket + lock;
- выбрать RCMU;
- зафиксировать электрическую схему и BOM.

Gate:

- все экземпляры имеют одинаковую документированную RS485-конфигурацию;
- известен способ изменения slave address;
- известен режим socket/lock/RCMU.

### Этап T1 — протокол EKEPC2 отдельно

Стенд USB-RS485 -> EKEPC2.

Проверить:

- фактическую скорость 9600/38400;
- 8N1;
- FC03 reads;
- FC06 writes;
- registers 100, 109, 110, 112, 114, 127, 140..157;
- endian;
- register numbering 0-based vs documentation;
- реакцию на address change + reboot;
- persistence after power loss;
- register 89 semantics;
- PWM 10/16/20/25/32 A осциллографом на CP, а не только readback register;
- state 141 mapping фактическим подключением EV/EV simulator.

Gate: собственная подтверждённая `registers_v<fw>.go` карта.

### Этап T2 — общий RS485 с MR6C

```text
WB-MGE:502 -> RS485-1 9600 8N1 -> MR6C + EKEPC2
```

Проверить:

- оба slave стабильно читаются по разным Unit ID;
- 10k+ смешанных транзакций без неправильной адресации;
- p50/p95/p99;
- CRC/error counters анализатором;
- EKEPC2 не генерирует самостоятельный Modbus master traffic;
- EKEPC2 traffic не продлевает MR6C watchdog;
- второй TCP client не нарушает safety model;
- WB-MGE sniffer/cache/multimaster отключены в production profile.

Gate: общая шина признана допустимой либо архитектурно разделяется.

### Этап T3 — `internal/modbusbus`

Код:

- единый connection owner;
- atomic unit-id transaction;
- priority queue;
- request deadline;
- reconnect/backoff;
- bus metrics;
- per-device health;
- cancellation;
- fairness (EKEPC2 не должен вытеснить MR6C watchdog).

Tests:

- concurrent callers;
- reconnect mid-request;
- stale response;
- timeout;
- wrong unit id response;
- 100 simulated posts.

Gate: race test + soak test.

### Этап T4 — EKEPC2 driver + simulator

Реализовать файлы раздела 6 и simulator раздела 12.

Gate:

- protocol unit tests;
- integration tests через multi-slave Modbus TCP simulator;
- hardware bench соответствует simulator behavior.

### Этап T5 — новая post state machine

Добавить EVSE observation и последовательности ON/OFF/fault.

Gate:

- питание невозможно подать только software-командой MR6C без permit EKEPC2;
- при CP fault/RCMU fault Kx физически отпадает;
- после fault автоматического ON нет;
- reboot Pi не восстанавливает зарядку автоматически;
- reboot EKEPC2 во время grant приводит в безопасное состояние.

### Этап T6 — Type 2 electrical validation

Проверить EV simulator/реальными автомобилями:

- no vehicle;
- cable connected;
- vehicle connected;
- charging requested;
- unplug sequence;
- lock/unlock;
- PP 13/20/32/63 A cables (какие доступны);
- CP diode check;
- emergency stop;
- RCMU AC/DC test согласно выбранному модулю;
- welded/stuck contactor simulation;
- PE fault tests выполняются только квалифицированным стендом/лабораторией.

### Этап T7 — dynamic current control

После стабильной фиксированной 32/16 A версии:

- API `SetCurrentLimit(A)`;
- clamp 6..32 A и PP cable limit;
- conversion A -> register 109;
- verify register 152;
- ramp/rate limit;
- fail-safe fallback current;
- 2 x EKEPC2 shared 32 A feeder allocator.

Gate для `2 x 16 A`:

- две EV одновременно;
- одна EV не берёт больше requested PWM;
- перераспределение не превышает feeder limit;
- потеря связи не может оставить обе на 32 A;
- allocator резервирует безопасно при stale state.

### Этап T8 — endurance

Минимум:

- 72 h непрерывный mixed Modbus soak;
- 10k+ connect/start/stop state transitions simulator;
- 1000+ реальных контакторных циклов в допустимом режиме;
- random power-cycle WB-MGE/MR6C/EKEPC2/Pi;
- Ethernet flap;
- RS485 disconnect/short simulation безопасным способом;
- reboot backend/uplink loss;
- temperature range по возможности климатического стенда.

### Этап T9 — pilot

1–3 реальных поста, ограниченный круг пользователей.

Собирать:

- bus utilization;
- Modbus timeout/CRC/exception;
- MR6C watchdog trips;
- EKEPC2 state transitions;
- RCMU events;
- lock faults;
- PWM commanded/readback;
- Kx feedback latency;
- session energy consistency.

### Этап T10 — production release

До выпуска:

- BOM с approved alternates;
- firmware whitelist EKEPC2;
- firmware whitelist MR6C;
- WB-MGE exported config/profile;
- адресный план;
- commissioning CLI;
- factory acceptance test;
- field replacement procedure;
- rollback strategy;
- electrical schematic revision;
- service diagnostics document;
- подтверждение применимых требований IEC/ГОСТ/ПУЭ и сертификации конечного изделия профильным специалистом/лабораторией.

## 15. Изменения в структуре репозитория

Целевая структура:

```text
cmd/
  line-controller/
  mr6c-bench/
  ekepc2-bench/
  evse-bus-bench/          # общий MR6C+EKEPC2 soak/latency/watchdog

internal/
  modbusbus/
    bus.go
    scheduler.go
    reconnect.go
    metrics.go

  hardware/
    mr6c/
      ...                  # перевод на injected modbusbus.Bus
    ekepc2/
      device.go
      registers_v1002.go
      config.go
      state.go
      current.go
      verify.go

  evse/
    interface.go
    observation.go
    current_limit.go

  controller/
    ...                    # coordinated MR6C + EVSE transitions

  sim/
    modbus_multi/
    mr6c/
    ekepc2/
```

Интерфейс EVSE, чтобы позже заменить EKEPC2 без переписывания controller:

```go
type EVSE interface {
    Observe(ctx context.Context) (Observation, error)
    VerifyConfig(ctx context.Context) error
    SetCurrentLimit(ctx context.Context, amps float64) error
}
```

Не включать в общий интерфейс vendor-specific register numbers.

## 16. Приёмочные тесты общей шины

Обязательный чек-лист:

- [ ] MR6C + EKEPC2 работают на одном RS485-1 при 9600 8N1.
- [ ] Уникальные slave IDs.
- [ ] EKEPC2 не master'ит шину.
- [ ] Нет cross-unit запросов при конкурентном software load.
- [ ] Один bus owner / scheduler.
- [ ] MR6C watchdog срабатывает, когда запросы MR6C остановлены, даже если EKEPC2 продолжает опрашиваться.
- [ ] Kill/STOP line-controller приводит Kx в OFF.
- [ ] Потеря Ethernet приводит Kx в OFF согласно watchdog.
- [ ] Потеря EKEPC2 -> новые ON запрещены; активная линия безопасно отключается.
- [ ] RCMU fault локально снимает EKEPC2 permit.
- [ ] CP fault локально снимает EKEPC2 permit.
- [ ] MR6C permit OFF всегда отключает Kx независимо от EKEPC2.
- [ ] PWM 16 A и 32 A измерен на CP и соответствует IEC 61851.
- [ ] PP cable limit не может быть превышен software limit.
- [ ] lock feedback fault запрещает старт.
- [ ] firmware mismatch запрещает новые ON.
- [ ] config mismatch запрещает новые ON.
- [ ] после reboot никакого автоматического восстановления ON.

## 17. Решение на текущий момент

Для стенда принять общую RS485-1 шину как **кандидат**, а не как уже доказанное production-решение:

```text
WB-MGE RS485-1, Modbus TCP :502, 9600 8N1
   ├── MR6C      unique slave ID
   └── EKEPC2-S  unique slave ID
```

Это самый дешёвый вариант, потому что не требует второго Ethernet/RS485 шлюза. Он становится production-вариантом только после двух ключевых доказательств:

1. полоса 9600 bit/s достаточна для MR6C watchdog/control + EKEPC2 polling с приемлемым p99;
2. трафик EKEPC2 не поддерживает watchdog MR6C живым при фактической остановке управления MR6C.

Если любое условие не выполняется, правильный fallback — физически отделить EKEPC2 на отдельный RS485 segment/gateway, не ослабляя safety/watchdog ради экономии одного интерфейса.

