package rtu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"
)

var (
	// ErrTimeout — ответ не набран за Timing.Response. Устройство молчит,
	// либо не приняло запрос (на ошибку адреса или CRC оно не отвечает).
	// TCP-сессия при этом исправна.
	ErrTimeout = errors.New("rtu: response timeout")
	// ErrGatewayBusy — шлюз закрыл соединение до первого ответа. В прозрачном
	// режиме WB-MGE так отклоняет второго клиента: порт занят другим мастером.
	ErrGatewayBusy = errors.New("rtu: gateway closed connection before first response (port busy)")
	// ErrClosed — шлюз закрыл соединение после успешного обмена.
	ErrClosed = errors.New("rtu: connection closed by gateway")
)

// StatusLen — длина ответа-состояния: адрес, байт состояния, CRC.
const StatusLen = 4

// drainWindow — сколько ждать остатка во входном буфере перед запросом.
// Нулевой дедлайн не годится: чтение с истёкшим дедлайном не отдаёт даже
// уже принятые байты.
const drainWindow = time.Millisecond

// Timing — тайминги транзакции. Значения для 9600 бит/с — DefaultTiming.
type Timing struct {
	Connect    time.Duration // таймаут TCP-подключения к шлюзу
	Response   time.Duration // ожидание ответа целиком, 300 мс при 9600
	FrameGap   time.Duration // пауза перед следующим запросом, ≥ 5 мс при 9600
	StatusTail time.Duration // ожидание «хвоста» после 4 байт с верным CRC, ~20 мс
}

// DefaultTiming — 9600 8N1 через WB-MGE.
func DefaultTiming() Timing {
	return Timing{
		Connect:    3 * time.Second,
		Response:   300 * time.Millisecond,
		FrameGap:   5 * time.Millisecond,
		StatusTail: 20 * time.Millisecond,
	}
}

func (t Timing) withDefaults() Timing {
	d := DefaultTiming()
	if t.Connect <= 0 {
		t.Connect = d.Connect
	}
	if t.Response <= 0 {
		t.Response = d.Response
	}
	if t.FrameGap < 0 {
		t.FrameGap = 0
	}
	if t.StatusTail <= 0 {
		t.StatusTail = d.StatusTail
	}
	return t
}

// Conn — одна транзакция «запрос-ответ» на прозрачном мосту. Не потокобезопасен:
// владелец — goroutine адаптера.
type Conn interface {
	// Transact отправляет кадр и читает ответ длиной expectLen байт или
	// ответ-состояние (StatusLen байт с верным CRC). CRC длинного ответа и
	// адрес проверяет вызывающий. При ErrTimeout возвращаются принятые байты.
	Transact(ctx context.Context, req []byte, expectLen int) ([]byte, error)
	Close() error
}

type Dialer func(ctx context.Context) (Conn, error)

// Stats — сведения о последней транзакции, для стенда (сегментация TCP у шлюза).
type Stats struct {
	Segments  int // сколько чтений понадобилось, чтобы собрать ответ
	Discarded int // байт мусора, сброшенных перед запросом
}

// StatsReporter реализуют соединения, умеющие отдавать Stats.
type StatsReporter interface {
	LastStats() Stats
}

// TCPDialer — прозрачный мост WB-MGE (порт RS485-2, по умолчанию 503).
func TCPDialer(addr string, t Timing) Dialer {
	t = t.withDefaults()
	return func(ctx context.Context) (Conn, error) {
		d := net.Dialer{Timeout: t.Connect, KeepAlive: 5 * time.Second}
		nc, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("rtu: connect %s: %w", addr, err)
		}
		if tc, ok := nc.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		return NewConn(nc, t), nil
	}
}

// NewConn оборачивает готовое соединение (TCP или net.Pipe в тестах).
func NewConn(nc net.Conn, t Timing) Conn {
	return &conn{nc: nc, t: t.withDefaults()}
}

type conn struct {
	nc       net.Conn
	t        Timing
	last     time.Time // конец предыдущей транзакции
	answered bool      // был хотя бы один полный ответ
	stats    Stats
}

func (c *conn) Close() error     { return c.nc.Close() }
func (c *conn) LastStats() Stats { return c.stats }

func (c *conn) Transact(ctx context.Context, req []byte, expectLen int) (resp []byte, err error) {
	if expectLen < StatusLen {
		return nil, fmt.Errorf("rtu: expectLen %d < %d", expectLen, StatusLen)
	}
	c.stats = Stats{}
	defer func() { c.last = time.Now() }()

	// Отмена ctx прерывает блокирующее чтение или запись.
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = c.nc.SetDeadline(time.Now())
		close(cancelled)
	})
	defer func() {
		if !stop() {
			<-cancelled // дождаться SetDeadline, чтобы он не сбил дедлайны следующей транзакции
		}
	}()
	wrap := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return c.classify(err)
	}

	// 1. Остаток от прошлой транзакции (опоздавший ответ) — мусор.
	if err := c.drain(); err != nil {
		return nil, wrap(err)
	}
	// 2. Системный таймаут устройства после предыдущего ответа.
	if wait := c.t.FrameGap - time.Since(c.last); wait > 0 && !c.last.IsZero() {
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		}
	}
	// 3. Кадр целиком одним Write.
	deadline := time.Now().Add(c.t.Response)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.nc.SetWriteDeadline(deadline); err != nil {
		return nil, wrap(err)
	}
	if _, err := c.nc.Write(req); err != nil {
		return nil, wrap(err)
	}
	// 4–5. Читать до expectLen; 4 байта с верным CRC — возможно, ответ-состояние.
	buf := make([]byte, expectLen)
	n := 0
	for n < expectLen {
		rd := deadline
		status := n == StatusLen && expectLen > StatusLen && CheckCRC(buf[:n])
		if status {
			if tail := time.Now().Add(c.t.StatusTail); tail.Before(deadline) {
				rd = tail
			}
		}
		if err := c.nc.SetReadDeadline(rd); err != nil {
			return buf[:n], wrap(err)
		}
		k, err := c.nc.Read(buf[n:])
		if k > 0 {
			n += k
			c.stats.Segments++
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return buf[:n], ctx.Err()
		}
		if isTimeout(err) {
			if status && n == StatusLen {
				c.answered = true
				return buf[:n], nil
			}
			return buf[:n], fmt.Errorf("%w: %d of %d bytes", ErrTimeout, n, expectLen)
		}
		return buf[:n], c.classify(err)
	}
	c.answered = true
	return buf, nil
}

// drain сбрасывает байты, пришедшие вне транзакции.
func (c *conn) drain() error {
	var tmp [64]byte
	for {
		if err := c.nc.SetReadDeadline(time.Now().Add(drainWindow)); err != nil {
			return err
		}
		k, err := c.nc.Read(tmp[:])
		c.stats.Discarded += k
		if err != nil {
			if isTimeout(err) {
				return nil
			}
			return err
		}
	}
}

func (c *conn) classify(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		if !c.answered {
			return fmt.Errorf("%w: %w", ErrGatewayBusy, err)
		}
		return fmt.Errorf("%w: %w", ErrClosed, err)
	}
	return err
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
