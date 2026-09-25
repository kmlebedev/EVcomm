// Command mercury230-bench — стенд «Go → WB-MGE (прозрачный мост, RS485-2) → Меркурий 230»:
// проверки этапа 1 из раздела 10 архитектуры модуля, задержки p50/p99, наблюдение
// за энергией под нагрузкой. Утилита шлёт только команды чтения и управления каналом.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kmlebedev/EVcomm/internal/controller"
	"github.com/kmlebedev/EVcomm/internal/domain"
	"github.com/kmlebedev/EVcomm/internal/hardware/mercury230"
	"github.com/kmlebedev/EVcomm/internal/registry"
	"github.com/kmlebedev/EVcomm/internal/rtu"
)

const usage = `mercury230-bench [флаги] <команда> [флаги команды]

Команды (номера — проверки раздела 10 архитектуры):
  check [-n 200]           автоматические проверки 1–5, 9–11; код выхода 1 при FAIL
  identity                 паспорт: серийный номер, дата, ПО, вариант исполнения (3)
  read                     разовое чтение P, AP1–AP3, A+ total
  passwords                какая кодировка пароля принята: 01×6 или ASCII 31×6 (2)
  power                    матрица «уровень доступа × 08 16 00 / 08 16 01 / 08 11 01» (5)
  latency [-n 10000]       задержки p50/p99 08 16 и 05 60 00, ошибки CRC, сегментация TCP (9, 10)
  slot                     второй TCP-клиент на порт моста, освобождение слота (11)
  expiry [-wait 250s]      истечение канала: код 5 и автоматическое переоткрытие (12)
  watch [-interval 15s] [-duration 0] [-csv file]
                           наблюдение под нагрузкой: ΔAPn, ΣΔAPn и ΔTotal, дискретность,
                           пропадание питания (6, 7, 8, 13)
  raw -len N <hex pdu>     произвольный запрос чтения, например: raw -len 15 081601

Параметры счётчика берутся из реестра (-registry, -post), флаги их переопределяют.
Пароль: -password, иначе переменная из password_env реестра; пароль уровня 2 для
проверки 5 — -password2 или MERCURY230_PASSWORD2. 12 hex-символов, 6 сырых байт.

Флаги:
`

type options struct {
	gateway   string
	address   uint8
	level     uint8
	password  *[6]byte
	password2 *[6]byte
	serial    string
	bwri      byte
	timing    rtu.Timing
	phaseMap  [3]domain.LineID
	verbose   bool
	dial      rtu.Dialer
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, nil))
}

// run — вся утилита; dial != nil подменяет подключение (тесты).
func run(ctx context.Context, args []string, stdout, stderr io.Writer, dial rtu.Dialer) int {
	fs := flag.NewFlagSet("mercury230-bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage); fs.PrintDefaults() }
	regPath := fs.String("registry", "configs/bench.yaml", "post registry (empty — only flags)")
	postID := fs.String("post", "", "post id (default: first post with a mercury230 meter)")
	gateway := fs.String("gateway", "", "WB-MGE RS485-2 transparent bridge host:port (overrides registry)")
	address := fs.Uint("address", 0, "meter address 1..240 (overrides registry)")
	level := fs.Uint("level", 0, "access level 1 or 2 (overrides registry)")
	password := fs.String("password", "", "password, 12 hex chars (overrides password_env)")
	password2 := fs.String("password2", os.Getenv("MERCURY230_PASSWORD2"), "level 2 password for check 5, 12 hex chars")
	serial := fs.String("serial", "", "expected serial number (overrides registry)")
	bwri := fs.Int("bwri", -1, "BWRI for 08 16 (overrides registry group_power_bwri)")
	timeout := fs.Duration("timeout", 0, "response timeout (overrides registry response_timeout)")
	verbose := fs.Bool("v", false, "print every exchange (hex, password masked)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}

	opt := options{level: 1, timing: rtu.DefaultTiming(), phaseMap: [3]domain.LineID{domain.LineL1, domain.LineL2, domain.LineL3}}
	if *regPath != "" {
		if err := fromRegistry(&opt, *regPath, *postID, stderr); err != nil && (*gateway == "" || !errors.Is(err, os.ErrNotExist)) {
			fmt.Fprintln(stderr, "mercury230-bench:", err)
			return 2
		}
	}
	if *gateway != "" {
		opt.gateway = *gateway
	}
	if *address != 0 {
		opt.address = uint8(*address)
	}
	if *level != 0 {
		opt.level = uint8(*level)
	}
	if *serial != "" {
		opt.serial = *serial
	}
	if *bwri >= 0 {
		opt.bwri = byte(*bwri)
	}
	if *timeout > 0 {
		opt.timing.Response = *timeout
	}
	for _, p := range []struct {
		s   string
		dst **[6]byte
	}{{*password, &opt.password}, {*password2, &opt.password2}} {
		if p.s == "" {
			continue
		}
		pw, err := mercury230.ParsePassword(p.s)
		if err != nil {
			fmt.Fprintln(stderr, "mercury230-bench:", err)
			return 2
		}
		*p.dst = &pw
	}
	opt.verbose = *verbose
	switch {
	case dial != nil:
		opt.dial = dial
	case opt.gateway == "":
		fmt.Fprintln(stderr, "mercury230-bench: no gateway: set -gateway or a mercury230 meter in the registry")
		return 2
	default:
		opt.dial = rtu.TCPDialer(opt.gateway, opt.timing)
	}
	if opt.address < 1 || opt.address > 240 {
		fmt.Fprintln(stderr, "mercury230-bench: meter address must be 1..240 (-address)")
		return 2
	}
	if opt.level != 1 && opt.level != 2 {
		fmt.Fprintln(stderr, "mercury230-bench: access level must be 1 or 2")
		return 2
	}

	b := &bench{opt: opt, out: stdout}
	fmt.Fprintf(stdout, "Счётчик: мост %s, адрес %d, уровень %d, пароль %s, ожидаемый номер %q, BWRI 08 16 = %02Xh, таймаут ответа %s\n\n",
		opt.gateway, opt.address, opt.level, describePassword(opt.password), opt.serial, opt.bwri, opt.timing.Response)

	cmd, rest := fs.Arg(0), fs.Args()[1:]
	var err error
	switch cmd {
	case "check":
		cf := flag.NewFlagSet("check", flag.ContinueOnError)
		n := cf.Int("n", 200, "transactions per request type in the latency check")
		if cf.Parse(rest) != nil {
			return 2
		}
		if failed := b.check(ctx, *n); failed {
			return 1
		}
		return 0
	case "identity":
		err = b.identity(ctx)
	case "read":
		err = b.read(ctx)
	case "passwords":
		err = b.withSession(ctx, false, func(s *session) error { _, err := b.checkPassword(ctx, s, &report{out: b.out}); return err })
	case "power":
		err = b.withSession(ctx, true, func(s *session) error { b.checkPower(ctx, s, &report{out: b.out}); return nil })
	case "latency":
		cf := flag.NewFlagSet("latency", flag.ContinueOnError)
		n := cf.Int("n", 10000, "transactions per request type")
		if cf.Parse(rest) != nil {
			return 2
		}
		err = b.withSession(ctx, true, func(s *session) error {
			r := &report{out: b.out}
			b.checkLatency(ctx, s, r, *n, b.probe(ctx, s))
			if r.failed() {
				return errors.New("latency criteria not met")
			}
			return nil
		})
	case "slot":
		err = b.withSession(ctx, false, func(s *session) error {
			r := &report{out: b.out}
			b.checkSlot(ctx, s, r)
			if r.failed() {
				return errors.New("slot check failed")
			}
			return nil
		})
	case "expiry":
		cf := flag.NewFlagSet("expiry", flag.ContinueOnError)
		wait := cf.Duration("wait", 250*time.Second, "idle time without exchange (channel lives 240 s)")
		if cf.Parse(rest) != nil {
			return 2
		}
		err = b.expiry(ctx, *wait)
	case "watch":
		cf := flag.NewFlagSet("watch", flag.ContinueOnError)
		interval := cf.Duration("interval", 15*time.Second, "energy snapshot period")
		duration := cf.Duration("duration", 0, "stop after (0 — until Ctrl-C)")
		csvPath := cf.String("csv", "", "also write rows to CSV file")
		if cf.Parse(rest) != nil {
			return 2
		}
		err = b.watch(ctx, *interval, *duration, *csvPath)
	case "raw":
		cf := flag.NewFlagSet("raw", flag.ContinueOnError)
		n := cf.Int("len", 0, "expected response length, bytes, with address and CRC")
		if cf.Parse(rest) != nil {
			return 2
		}
		err = b.raw(ctx, *n, cf.Args())
	default:
		fs.Usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(stdout, "ошибка:", err)
		return 1
	}
	return 0
}

func fromRegistry(opt *options, path, postID string, stderr io.Writer) error {
	reg, err := registry.Load(path)
	if err != nil {
		return err
	}
	var post registry.Post
	found := false
	for _, p := range reg.Posts {
		if (postID == "" || p.ID == domain.PostID(postID)) && p.Meter != nil && p.Meter.Driver == registry.MeterMercury230 {
			post, found = p, true
			break
		}
	}
	if !found {
		if postID != "" {
			return fmt.Errorf("post %q with a mercury230 meter not found in %s", postID, path)
		}
		return nil
	}
	m := post.Meter
	opt.gateway, opt.address, opt.level, opt.serial, opt.bwri = m.Gateway, m.Address, m.AccessLevel, m.Serial, m.GroupPowerBWRI
	opt.timing = controller.MeterTiming(*m)
	copy(opt.phaseMap[:], m.PhaseMap)
	if pw, err := m.Password(); err == nil {
		opt.password = &pw
	} else {
		fmt.Fprintf(stderr, "пост %s: %v — будет подобрана кодировка пароля по умолчанию\n", post.ID, err)
	}
	return nil
}

type bench struct {
	opt  options
	out  io.Writer
	onEx func(mercury230.Exchange) // сбор транзакций для отдельных проверок
}

func (b *bench) printf(format string, a ...any) { fmt.Fprintf(b.out, format, a...) }

func (b *bench) trace(ex mercury230.Exchange) {
	if b.opt.verbose {
		status := "ok"
		if ex.Err != nil {
			status = ex.Err.Error()
		}
		b.printf("    %-16s → % X  ← % X  %s  %s\n", ex.Request, ex.Req, ex.Resp, ex.Duration.Round(100*time.Microsecond), status)
	}
	if b.onEx != nil {
		b.onEx(ex)
	}
}

// session — одно TCP-соединение с мостом и сессия протокола над ним.
type session struct {
	conn  rtu.Conn
	m     *mercury230.Meter
	level uint8
	pw    [6]byte
}

func (b *bench) connect(ctx context.Context) (*session, error) {
	conn, err := b.opt.dial(ctx)
	if err != nil {
		return nil, err
	}
	s := &session{conn: conn, level: b.opt.level}
	if b.opt.password != nil {
		s.pw = *b.opt.password
	}
	s.m = mercury230.NewMeter(conn, mercury230.MeterConfig{Address: b.opt.address, AccessLevel: s.level, Password: s.pw, Trace: b.trace})
	return s, nil
}

func (s *session) close() {
	cctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = s.m.CloseChannel(cctx)
	_ = s.conn.Close()
}

// withSession подключается и, если open, открывает канал (с подбором кодировки пароля).
func (b *bench) withSession(ctx context.Context, open bool, f func(*session) error) error {
	s, err := b.connect(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	if open {
		if b.opt.password == nil {
			if _, err := b.checkPassword(ctx, s, &report{out: io.Discard}); err != nil {
				return err
			}
		} else if err := s.m.Open(ctx); err != nil {
			return err
		}
	}
	return f(s)
}

func (b *bench) adapterConfig() mercury230.Config {
	cfg := mercury230.Config{
		Name: "bench", Dial: b.opt.dial, Address: b.opt.address, AccessLevel: b.opt.level,
		ExpectedSerial: b.opt.serial, PhaseMap: b.opt.phaseMap, GroupPowerBWRI: b.opt.bwri,
		ReconnectMin: 500 * time.Millisecond, ReconnectMax: 5 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if b.opt.password != nil {
		cfg.Password = *b.opt.password
	}
	if b.opt.verbose {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return cfg
}

func describePassword(pw *[6]byte) string {
	if pw == nil {
		return "не задан"
	}
	same := func(v byte) bool {
		for _, c := range pw {
			if c != v {
				return false
			}
		}
		return true
	}
	switch {
	case same(0x01):
		return "01×6 (двоичный «111111»)"
	case same(0x02):
		return "02×6 (двоичный «222222»)"
	case same(0x31):
		return "31×6 (ASCII «111111»)"
	case same(0x32):
		return "32×6 (ASCII «222222»)"
	}
	return "задан (6 байт)"
}
