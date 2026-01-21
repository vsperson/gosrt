# Проблемы gosrt и причины перехода на libsrt

Документ описывает проблемы библиотеки [datarhei/gosrt](https://github.com/datarhei/gosrt) (Pure Go реализация SRT), обнаруженные при разработке TSA, и обоснование параллельного использования [Haivision/srtgo](https://github.com/Haivision/srtgo) (CGO wrapper над официальной libsrt).

**Версия gosrt:** github.com/datarhei/gosrt v0.6.0+  
**Версия libsrt:** github.com/haivision/srtgo + libsrt 1.5.x

---

## Критические проблемы

### 1. Дропы пакетов из-за фиксированного буфера (CRITICAL)

**Симптом:**
- На потоке `srt://91.234.172.202:9020` за 30 секунд:
  - gosrt: **293 dropped packets** ⚠️
  - libsrt: **0 dropped packets** ✅

**Причина в коде gosrt (`connection.go`):**

```go
// Создание буфера с фиксированным размером
c.readQueue = make(chan packet.Packet, 1024) // Всего 1024 пакета!

// Non-blocking запись в буфер
select {
case <-c.ctx.Done():
case c.readQueue <- p:
default:
    // ДРОП ПАКЕТА если канал полон!
    c.log("connection:error", func() string { 
        return "readQueue was blocking, dropping packet" 
    })
}
```

**Проблема:**
1. Пакеты приходят из сети быстрее, чем приложение их читает
2. Канал `readQueue` (размер всего 1024 пакета) заполняется
3. При попытке записи следующего пакета срабатывает `default` case
4. Пакет **дропается без ожидания** - потеря данных!

**Почему libsrt работает корректно:**
- Использует системную буферизацию на уровне ядра ОС
- Блокирующие вызовы с корректным flow control
- Механизмы backpressure предотвращают потери
- Буферы настраиваемого размера (до 64MB)

**Влияние на качество:**
- Невосстановимые потери пакетов (не через сеть, а на уровне приложения)
- Ложные события SRTPacketDrop, искажающие реальную картину
- Артефакты в видео/аудио потоках

---

### 2. Отсутствие настраиваемого размера буфера чтения

**Проблема:**
Параметр `ReadQueueSize` отсутствует в `srt.Config`. Невозможно увеличить буфер без модификации исходного кода библиотеки.

**Требуемое изменение в gosrt:**

```go
// config.go
type Config struct {
    // ... existing fields ...
    
    // ReadQueueSize specifies the size of the read queue buffer.
    // Default: 1024
    // Recommended: 8192-16384 for high-bitrate streams
    ReadQueueSize int
}

func DefaultConfig() Config {
    return Config{
        // ...
        ReadQueueSize: 1024, // Для совместимости
    }
}

// connection.go
queueSize := c.config.ReadQueueSize
if queueSize <= 0 {
    queueSize = 1024
}
c.readQueue = make(chan packet.Packet, queueSize)
```

**Объем изменений:** ~10-15 строк кода

---

### 3. Некорректная статистика Interval (ненакопительная)

**Симптом:**
Значения `stats.Interval.*` в gosrt показывают **мгновенные** значения за последний интервал, но:
- Нет возможности получить точный размер интервала
- Некоторые поля могут быть 0 между опросами
- Несоответствие с документацией libsrt

**Пример несоответствия:**

```go
// gosrt возвращает
data.Interval.PktRecvRetrans = stats.Interval.PktRecvRetrans  // Может быть 0

// libsrt возвращает
data.Interval.PktRecvRetrans = stats.PktRcvRetrans  // Накопительное значение
```

**Решение в TSA:**
Использование `Accumulated` данных и расчёт дельты между опросами:

```go
// Вместо Interval используем дельту Accumulated
deltaLoss := stats.Accumulated.PktRecvLoss - m.lastStats.Accumulated.PktRecvLoss
deltaDrop := stats.Accumulated.PktRecvDrop - m.lastStats.Accumulated.PktRecvDrop
```

---

### 4. Отсутствие некоторых метрик

**Отсутствующие или неточные поля в gosrt:**

| Метрика | gosrt | libsrt | Комментарий |
|---------|-------|--------|-------------|
| `PktRecvRetrans` (Total) | ❌ Только Interval | ✅ Полный | Требуется для расчёта recovered |
| `ByteRecvRetrans` (Total) | ❌ Отсутствует | ✅ Полный | Требуется для статистики |
| `SocketId` | ✅ Есть | ✅ Есть | - |
| `PeerSocketId` | ✅ Есть | ✅ Есть | - |
| `StreamId` | ✅ Есть | ⚠️ Требует хранения | Не в stats напрямую |

**Решение в TSA для libsrt:**

```go
// Накапливаем самостоятельно
r.totalPktRecvRetrans += uint64(stats.PktRcvRetrans)
r.totalByteRecvRetrans += uint64(stats.ByteRetrans)

data.Accumulated.PktRecvRetrans = r.totalPktRecvRetrans
data.Accumulated.ByteRecvRetrans = r.totalByteRecvRetrans
```

---

## Средние проблемы

### 5. Нет встроенного механизма reconnect

**Проблема:**
При разрыве соединения gosrt не предоставляет автоматический reconnect. Приложение должно:
1. Обнаружить разрыв (через ошибку Read)
2. Закрыть старое соединение
3. Создать новое подключение
4. Обработать все edge cases

**Влияние:**
- Усложнение кода приложения
- Потенциальные утечки горутин при неправильной обработке
- Время простоя при переподключении

---

### 6. Ограниченная обработка ошибок подключения

**Проблема:**
Ошибки подключения в gosrt не всегда детализированы:

```go
// gosrt возвращает общую ошибку
err := srt.Dial("srt", host, config)
// err.Error() = "connection failed" - неинформативно
```

**Требуется:**
- Различать timeout vs rejected vs handshake failed
- Получать причину отклонения от сервера
- Классифицировать ошибки для событий

**Решение в TSA:**
Парсинг текста ошибки для классификации:

```go
errStr := err.Error()
if contains(errStr, "handshake") {
    errorType = network.ErrorTypeSRTHandshakeFailed
} else if contains(errStr, "timeout") {
    errorType = network.ErrorTypeSRTConnectionTimeout
} else if contains(errStr, "refused") || contains(errStr, "rejected") {
    errorType = network.ErrorTypeSRTConnectionRejected
}
```

---

### 7. Проблемы с message mode

**Симптом:**
При работе в message mode (TSBPD) gosrt может возвращать неполные сообщения или требовать специфичной обработки.

**libsrt решение:**
Добавлен внутренний буфер для корректной сборки сообщений:

```go
type LibSRTReader struct {
    // ...
    buffer    []byte // Internal buffer for partial message data
    bufferPos int    // Current position in buffer
}
```

---

## Незначительные проблемы

### 8. Отсутствие статической сборки

**Проблема:**
gosrt - Pure Go, не требует CGO. Но при переходе на libsrt требуется:
- CGO=1
- Установленный libsrt на системе сборки
- Статическая линковка для переносимости

**Решение:**
Статическая сборка с musl:

```bash
CC=musl-gcc CGO_ENABLED=1 go build \
    -ldflags "-linkmode external -extldflags '-static'" \
    -o bin/tsa-agent-linux-amd64 ./cmd/tsa-agent
```

---

## Сравнительное тестирование

### Тест на srt://91.234.172.202:9020 (30 секунд)

| Метрика | gosrt | libsrt | Улучшение |
|---------|-------|--------|-----------|
| PktRecvDrop | 293 | 0 | **-100%** ✅ |
| PktRecvLoss | 571 | 290 | **-49%** |
| RTT | ~77ms | ~82ms | +6% |
| Bitrate | 2.60 Mbps | 2.93 Mbps | **+13%** |
| CPU usage | ~2% | ~3% | +50% |

**Вывод:** libsrt полностью решает проблему дропов и улучшает качество приёма.

---

## Рекомендации по доработке gosrt

### Приоритет 1: Критические изменения

#### 1.1 Настраиваемый ReadQueueSize

**Файлы:** `config.go`, `connection.go`

```go
// config.go
type Config struct {
    // ...
    ReadQueueSize int // Default: 1024, Recommended: 8192+
}

// connection.go
queueSize := c.config.ReadQueueSize
if queueSize <= 0 {
    queueSize = 1024
}
c.readQueue = make(chan packet.Packet, queueSize)
```

#### 1.2 Опциональный blocking write

**Файл:** `connection.go`

```go
if c.config.BlockOnFullQueue {
    // Блокируемся вместо дропа
    select {
    case <-c.ctx.Done():
        return
    case c.readQueue <- p:
    }
} else {
    // Текущее поведение - non-blocking с дропом
    select {
    case <-c.ctx.Done():
    case c.readQueue <- p:
    default:
        c.log("connection:error", ...)
    }
}
```

### Приоритет 2: Улучшения статистики

#### 2.1 Добавить накопительные счётчики

**Файл:** `statistics.go`

```go
type AccumulatedStatistics struct {
    // ... existing ...
    PktRecvRetrans uint64 // Добавить!
    ByteRecvRetrans uint64 // Добавить!
}
```

#### 2.2 Добавить длительность интервала

```go
type IntervalStatistics struct {
    MsInterval uint64 // Точная длительность интервала в мс
    // ...
}
```

### Приоритет 3: Улучшения API

#### 3.1 Структурированные ошибки

```go
type SRTError struct {
    Code    int    // Код ошибки SRT
    Type    string // handshake/timeout/rejected/etc
    Message string // Детальное описание
    Reason  string // Причина от сервера (для rejected)
}

func (e *SRTError) Error() string {
    return fmt.Sprintf("SRT %s: %s", e.Type, e.Message)
}
```

#### 3.2 Callbacks для событий

```go
type ConnectionCallbacks struct {
    OnConnected    func()
    OnDisconnected func(error)
    OnReconnecting func(attempt int)
    OnStatsUpdate  func(Statistics)
}
```

---

## Текущая стратегия TSA

### Гибридный подход

```go
// factory.go
switch scheme {
case "srt":
    return NewGoSRTReader(source)    // Pure Go, все платформы
case "libsrt":
    return NewLibSRTReader(source)   // CGO, Linux only, без дропов
}
```

### Использование

| Схема | Реализация | Платформы | Дропы | Рекомендация |
|-------|------------|-----------|-------|--------------|
| `srt://` | gosrt | Windows, macOS, Linux | Возможны | Для тестов и Windows |
| `libsrt://` | libsrt | Linux only | Нет | **Production на Linux** |

### Дефолт в UI

- Linux агенты: `libsrt://` (рекомендуется)
- Windows агенты: `srt://` (единственный вариант)
- macOS агенты: `srt://` (единственный вариант)

---

## Заключение

### Почему мы перешли на libsrt для Linux

1. **Критическая проблема дропов** - 293 потерянных пакета за 30 сек неприемлемо для production мониторинга
2. **Стабильность** - libsrt это официальная реализация от Haivision, используется в FFmpeg, OBS, VLC
3. **Полная статистика** - все метрики SRT доступны корректно
4. **Производительность** - несмотря на CGO overhead, качество приёма значительно лучше

### Что нужно для возврата к gosrt

1. **Обязательно:** Настраиваемый `ReadQueueSize` (минимум 8192)
2. **Желательно:** Опция blocking write вместо дропа
3. **Желательно:** Накопительные `PktRecvRetrans`/`ByteRecvRetrans`
4. **Опционально:** Структурированные ошибки

### Ссылки

- [datarhei/gosrt](https://github.com/datarhei/gosrt) - Pure Go SRT
- [Haivision/srt](https://github.com/Haivision/srt) - Официальная libsrt
- [Haivision/srtgo](https://github.com/Haivision/srtgo) - CGO wrapper
- [docs/specs/GOSRT_DROPS_ANALYSIS.md](specs/GOSRT_DROPS_ANALYSIS.md) - Детальный анализ дропов
- [docs/specs/LIBSRT_IMPLEMENTATION.md](specs/LIBSRT_IMPLEMENTATION.md) - План реализации libsrt
