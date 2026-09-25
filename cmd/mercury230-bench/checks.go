package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/kmlebedev/EVcomm/internal/hardware/mercury230"
	"github.com/kmlebedev/EVcomm/internal/rtu"
)

type verdict string

const (
	vPass verdict = "PASS"
	vFail verdict = "FAIL"
	vWarn verdict = "WARN"
	vInfo verdict = "INFO"
)

// report печатает результаты по мере выполнения проверок.
type report struct {
	out    io.Writer
	counts map[verdict]int
}

func (r *report) add(id, name string, v verdict, format string, a ...any) {
	if r.counts == nil {
		r.counts = map[verdict]int{}
	}
	r.counts[v]++
	fmt.Fprintf(r.out, "%-3s %-26s %-4s  %s\n", id, name, v, fmt.Sprintf(format, a...))
}

func (r *report) sub(format string, a ...any) {
	fmt.Fprintf(r.out, "      %s\n", fmt.Sprintf(format, a...))
}

func (r *report) failed() bool { return r.counts[vFail] > 0 }

func (r *report) summary() {
	fmt.Fprintf(r.out, "\nИтого: PASS %d, WARN %d, INFO %d, FAIL %d\n", r.counts[vPass], r.counts[vWarn], r.counts[vInfo], r.counts[vFail])
}

var passwordCandidates = [][6]byte{
	{0x01, 0x01, 0x01, 0x01, 0x01, 0x01}, // драйвер wb-mqtt-serial
	{0x31, 0x31, 0x31, 0x31, 0x31, 0x31}, // описание протокола 2023 года, ASCII
}

// check — автоматические проверки 1–5, 9–11.
func (b *bench) check(ctx context.Context, n int) (failed bool) {
	r := &report{out: b.out}
	defer r.summary()
	s, err := b.connect(ctx)
	if err != nil {
		r.add("1", "связь с мостом", vFail, "%v", err)
		return true
	}
	defer s.close()

	id, err := s.m.ReadSerial(ctx)
	switch {
	case err != nil:
		r.add("1", "08 00: серийный номер", vFail, "%v", err)
		if errors.Is(err, rtu.ErrGatewayBusy) {
			r.sub("порт моста занят другим мастером (wb-mqtt-serial, homeui, второй экземпляр Go)")
		} else {
			r.sub("проверить: режим порта RS485-2 «прозрачный мост», 9600 8N1, адрес счётчика, питание интерфейса")
		}
		return true
	case b.opt.serial != "" && id.Serial != b.opt.serial:
		r.add("1", "08 00: серийный номер", vFail, "%s ≠ реестр %s (сырые байты % X)", id.Serial, b.opt.serial, id.SerialRaw[:])
	default:
		r.add("1", "08 00: серийный номер", vPass, "%s, выпуск %s (сырые байты % X)", id.Serial, date(id.Manufactured), id.SerialRaw[:])
	}

	if ok, _ := b.checkPassword(ctx, s, r); !ok {
		return true
	}
	b.checkIdentity(ctx, s, r)
	b.checkPhaseEnergy(ctx, s, r)
	b.checkPower(ctx, s, r)
	if ctx.Err() == nil {
		b.checkLatency(ctx, s, r, n, b.probe(ctx, s))
	}
	if ctx.Err() == nil {
		b.checkSlot(ctx, s, r)
	}
	fmt.Fprint(b.out, `
Ручные и длительные проверки:
  6, 7, 8, 13  watch -interval 15s -duration 2h -csv run.csv   (нагрузка на фазы; питание счётчика — снять и вернуть)
  9            latency -n 10000
  12           expiry -wait 250s
`)
	return r.failed()
}

// checkPassword — проверка 2. Оставляет канал открытым с принятым паролем.
func (b *bench) checkPassword(ctx context.Context, s *session, r *report) (bool, error) {
	if b.opt.password != nil {
		pw := *b.opt.password
		err := s.m.OpenWith(ctx, b.opt.level, pw)
		if err == nil {
			s.use(b, b.opt.level, pw)
			r.add("2", "01h: пароль", vPass, "уровень %d принят, кодировка %s", b.opt.level, describePassword(&pw))
			return true, nil
		}
		if !mercury230.IsStatus(err, mercury230.StatusAccessDenied) {
			r.add("2", "01h: пароль", vFail, "%v", err)
			return false, err
		}
		r.add("2", "01h: пароль", vFail, "пароль уровня %d (%s) отклонён: код 3", b.opt.level, describePassword(&pw))
		for _, c := range passwordCandidates {
			if c != pw && s.m.OpenWith(ctx, 1, c) == nil {
				r.sub("на уровне 1 принят %s — записать его в password_env (hex %X)", describePassword(&c), c[:])
			}
		}
		return false, err
	}
	for _, c := range passwordCandidates {
		err := s.m.OpenWith(ctx, 1, c)
		if err == nil {
			b.opt.password, b.opt.level = &c, 1
			s.use(b, 1, c)
			r.add("2", "01h: пароль", vPass, "пароль не задан; уровень 1 принял %s — записать в password_env: %X", describePassword(&c), c[:])
			return true, nil
		}
		if !mercury230.IsStatus(err, mercury230.StatusAccessDenied) {
			r.add("2", "01h: пароль", vFail, "%v", err)
			return false, err
		}
	}
	r.add("2", "01h: пароль", vFail, "ни 01×6, ни ASCII 31×6 не приняты на уровне 1; задать пароль -password")
	return false, errors.New("no password accepted")
}

// use меняет уровень и пароль, с которыми Meter переоткрывает канал при коде 5.
func (s *session) use(b *bench, level uint8, pw [6]byte) {
	s.level, s.pw = level, pw
	s.m = mercury230.NewMeter(s.conn, mercury230.MeterConfig{Address: b.opt.address, AccessLevel: level, Password: pw, Trace: b.trace})
}

// checkIdentity — проверка 3.
func (b *bench) checkIdentity(ctx context.Context, s *session, r *report) {
	id, err := s.m.ReadIdentity(ctx)
	if err != nil {
		r.add("3", "08 01 00: паспорт", vFail, "%v", err)
		return
	}
	detail := fmt.Sprintf("номер %s, выпуск %s, ПО %q, вариант % X", id.Serial, date(id.Manufactured), id.Firmware, id.Variant[:])
	switch {
	case !id.HasVariant:
		r.add("3", "08 01 00: паспорт", vWarn, "%s — вариант исполнения не прочитан (08 01 00 и 08 12)", detail)
	case id.Variant.PhaseEnergy():
		r.add("3", "08 01 00: паспорт", vPass, "%s — пофазный учёт A+ есть", detail)
	default:
		r.add("3", "08 01 00: паспорт", vFail, "%s — бит пофазного учёта A+ = 0", detail)
	}
	if id.HasVariant {
		r.sub("интерфейс (байт 4, биты 3–2) = %d (1 — RS-485); бит суммирования фаз (байт 3, бит 7) = %v", id.Variant.Interface(), id.Variant.SumModeBit())
		r.sub("записать в реестр: ПО %s, вариант %X", id.Firmware, id.Variant[:])
	}
}

// checkPhaseEnergy — проверка 4.
func (b *bench) checkPhaseEnergy(ctx context.Context, s *session, r *report) {
	pe, ex, err := s.m.ReadPhaseEnergy(ctx)
	switch {
	case err == nil:
		r.add("4", "05 60 00: AP1–AP3", vPass, "уровень %d: %d / %d / %d Вт·ч", s.level, pe[0], pe[1], pe[2])
		r.sub("кадр ответа % X", ex.Resp)
	case errors.Is(err, mercury230.ErrMasked):
		r.add("4", "05 60 00: AP1–AP3", vFail, "маска FF FF FF FF: пофазный учёт не поддержан (кадр % X)", ex.Resp)
	default:
		r.add("4", "05 60 00: AP1–AP3", vFail, "уровень %d: %v", s.level, err)
	}
	if te, _, err := s.m.ReadTotalEnergy(ctx); err == nil {
		r.sub("05 00 00: A+ %d Вт·ч, A− %s, R+ %s, R− %s; ΣAPn = %d", te.APlus, masked(te.AMinus, te.Masked[1]),
			masked(te.RPlus, te.Masked[2]), masked(te.RMinus, te.Masked[3]), uint64(pe[0])+uint64(pe[1])+uint64(pe[2]))
	} else {
		r.sub("05 00 00: %v", err)
	}
}

func masked(v uint32, m bool) string {
	if m {
		return "маска"
	}
	return fmt.Sprint(v)
}

// checkPower — проверка 5: матрица уровней доступа и способов чтения P.
func (b *bench) checkPower(ctx context.Context, s *session, r *report) {
	reqs := []mercury230.Request{mercury230.ReadActivePower(0x00), mercury230.ReadActivePower(0x01), mercury230.ReadActivePowerPhase(1)}
	cfgIdx := int(b.opt.bwri)
	if b.opt.bwri > 1 {
		reqs = append(reqs, mercury230.ReadActivePower(b.opt.bwri))
		cfgIdx = len(reqs) - 1
	}
	passwords := map[uint8]*[6]byte{s.level: &s.pw}
	if s.level == 1 && b.opt.password2 != nil {
		passwords[2] = b.opt.password2
	}
	ok := map[uint8][]bool{}
	var lines []string
	for _, level := range []uint8{1, 2} {
		pw := passwords[level]
		if pw == nil {
			lines = append(lines, fmt.Sprintf("уровень %d: пароль не задан (-password2), пропущено", level))
			continue
		}
		if err := s.m.OpenWith(ctx, level, *pw); err != nil {
			lines = append(lines, fmt.Sprintf("уровень %d: канал не открыт: %v", level, err))
			continue
		}
		res := make([]bool, len(reqs))
		for i, req := range reqs {
			p, _, err := s.m.Do(ctx, req)
			res[i] = err == nil
			lines = append(lines, fmt.Sprintf("уровень %d  % X  %s", level, req.PDU, powerResult(p, err)))
		}
		ok[level] = res
	}
	if err := s.m.OpenWith(ctx, s.level, s.pw); err != nil {
		lines = append(lines, fmt.Sprintf("канал на уровне %d не переоткрыт: %v", s.level, err))
	}

	cfg := ok[s.level]
	group := func(res []bool) (bool, int) {
		for i := range 2 {
			if res != nil && res[i] {
				return true, i
			}
		}
		return false, 0
	}
	switch {
	case cfg != nil && cfg[cfgIdx]:
		r.add("5", "мгновенная P", vPass, "08 16 %02X работает на уровне %d", b.opt.bwri, s.level)
	case cfg != nil && func() bool { g, _ := group(cfg); return g }():
		_, i := group(cfg)
		r.add("5", "мгновенная P", vWarn, "08 16 %02X не работает, работает 08 16 %02X: задать group_power_bwri: %d", b.opt.bwri, i, i)
	case cfg != nil && cfg[2]:
		r.add("5", "мгновенная P", vWarn, "08 16 не работает на уровне %d; адаптер использует запасной путь 3 × 08 11", s.level)
	case s.level == 1 && ok[2] != nil && slices.Contains(ok[2], true):
		r.add("5", "мгновенная P", vWarn, "на уровне 1 недоступна, на уровне 2 доступна: access_level: 2 (пароль «хозяина», согласовать с метрологом)")
	default:
		r.add("5", "мгновенная P", vFail, "ни один способ не работает на доступных уровнях")
	}
	for _, l := range lines {
		r.sub("%s", l)
	}
}

func powerResult(p []byte, err error) string {
	if err != nil {
		return err.Error()
	}
	switch len(p) {
	case 12:
		pw, _ := mercury230.DecodeGroupPower(p)
		return fmt.Sprintf("P = %s, P1 %s, P2 %s, P3 %s", watts(pw.TotalCW), watts(pw.PhaseCW[0]), watts(pw.PhaseCW[1]), watts(pw.PhaseCW[2]))
	case 3:
		return "P1 = " + watts(mercury230.DecodePower3(p))
	}
	return fmt.Sprintf("данные % X", p)
}

func watts(cw int32) string { return fmt.Sprintf("%.2f Вт", float64(cw)/100) }

type plan struct {
	power  *mercury230.Request
	energy mercury230.Request
}

// probe выбирает рабочие запросы для серии задержек.
func (b *bench) probe(ctx context.Context, s *session) plan {
	p := plan{energy: mercury230.ReadPhaseEnergy()}
	if _, _, err := s.m.ReadPhaseEnergy(ctx); err != nil {
		p.energy = mercury230.ReadTotalEnergy()
	}
	if _, err := s.m.ReadGroupPower(ctx, b.opt.bwri); err == nil {
		req := mercury230.ReadActivePower(b.opt.bwri)
		p.power = &req
	} else if _, _, err := s.m.Do(ctx, mercury230.ReadActivePowerPhase(1)); err == nil {
		req := mercury230.ReadActivePowerPhase(1)
		p.power = &req
	}
	return p
}

// checkLatency — проверки 9 и 10.
func (b *bench) checkLatency(ctx context.Context, s *session, r *report, n int, p plan) {
	reqs := []mercury230.Request{p.energy}
	if p.power != nil {
		reqs = []mercury230.Request{*p.power, p.energy}
	}
	var exchanges, corrupt, timeouts, failures int
	b.onEx = func(ex mercury230.Exchange) {
		exchanges++
		switch {
		case errors.Is(ex.Err, mercury230.ErrCRC), errors.Is(ex.Err, mercury230.ErrAddress), errors.Is(ex.Err, mercury230.ErrLength):
			corrupt++
		case errors.Is(ex.Err, rtu.ErrTimeout):
			timeouts++
		}
	}
	defer func() { b.onEx = nil }()
	durations := map[string][]time.Duration{}
	segments := map[int]int{}
	sr, _ := s.conn.(rtu.StatsReporter)
	started := time.Now()
	var transportErr error
loop:
	for i := range n {
		for _, req := range reqs {
			t0 := time.Now()
			_, _, err := s.m.Do(ctx, req)
			d := time.Since(t0)
			if ctx.Err() != nil {
				break loop
			}
			if err != nil {
				failures++
				if mercury230.Classify(err) == mercury230.KindTransport {
					transportErr = err
					break loop
				}
				continue
			}
			durations[req.Name] = append(durations[req.Name], d)
			if sr != nil {
				segments[sr.LastStats().Segments]++
			}
		}
		if n >= 1000 && (i+1)%(n/10) == 0 {
			fmt.Fprintf(b.out, "… %d/%d за %s\n", i+1, n, time.Since(started).Round(time.Second))
		}
	}
	if exchanges == 0 {
		r.add("9", "задержки", vFail, "нет обмена: %v", transportErr)
		return
	}
	rate := func(k int) float64 { return 100 * float64(k) / float64(exchanges) }
	worst := time.Duration(0)
	var parts []string
	for _, req := range reqs {
		ds := durations[req.Name]
		parts = append(parts, fmt.Sprintf("% X: %s", req.PDU, pct(ds)))
		if len(ds) > 0 {
			worst = max(worst, at(ds, 0.99))
		}
	}
	v := vPass
	if transportErr != nil || worst >= b.opt.timing.Response || rate(corrupt) >= 0.1 || rate(timeouts) >= 0.1 {
		v = vFail
	}
	r.add("9", "задержки p50/p99/max", v, "%s", strings.Join(parts, "; "))
	r.sub("транзакций %d за %s; испорченных кадров %d (%.3f%%), таймаутов %d (%.3f%%), неудачных запросов %d; критерий: p99 < %s, ошибок < 0,1%%",
		exchanges, time.Since(started).Round(time.Millisecond), corrupt, rate(corrupt), timeouts, rate(timeouts), failures, b.opt.timing.Response)
	if transportErr != nil {
		r.sub("серия прервана: %v", transportErr)
	}

	if sr == nil {
		return
	}
	keys := slices.Sorted(func(yield func(int) bool) {
		for k := range segments {
			if !yield(k) {
				return
			}
		}
	})
	total := 0
	for _, c := range segments {
		total += c
	}
	var hist []string
	for _, k := range keys {
		hist = append(hist, fmt.Sprintf("%d сегм.: %.1f%%", k, 100*float64(segments[k])/float64(total)))
	}
	switch {
	case len(keys) == 0:
		r.add("10", "сегментация TCP", vInfo, "нет полных ответов")
	case keys[len(keys)-1] > 1 && corrupt == 0:
		r.add("10", "сегментация TCP", vPass, "ответ приходил частями (%s) и собирался без ошибок", strings.Join(hist, ", "))
	case keys[len(keys)-1] > 1:
		r.add("10", "сегментация TCP", vWarn, "ответ приходил частями (%s), испорченных кадров %d", strings.Join(hist, ", "), corrupt)
	default:
		r.add("10", "сегментация TCP", vInfo, "все ответы одним чтением (%s); подтвердить захватом трафика", strings.Join(hist, ", "))
	}
}

// checkSlot — проверка 11. Заменяет соединение сессии новым.
func (b *bench) checkSlot(ctx context.Context, s *session, r *report) {
	var second string
	secondServed := false
	conn2, err := b.opt.dial(ctx)
	if err != nil {
		second = fmt.Sprintf("подключение отклонено: %v", err)
	} else {
		m2 := mercury230.NewMeter(conn2, mercury230.MeterConfig{Address: b.opt.address, Trace: b.trace})
		_, err = m2.ReadSerial(ctx)
		_ = conn2.Close()
		switch {
		case err == nil:
			secondServed = true
			second = "второму клиенту ответил счётчик: порт не эксклюзивен"
		case errors.Is(err, rtu.ErrGatewayBusy):
			second = "подключение принято и сразу закрыто (как ожидается)"
		default:
			second = err.Error()
		}
	}
	_, firstErr := s.m.ReadSerial(ctx)

	// Штатное закрытие (02h + FIN) должно освобождать слот сразу.
	s.close()
	t0 := time.Now()
	var release time.Duration
	var lastErr error
	for time.Since(t0) < 25*time.Second && ctx.Err() == nil {
		c, err := b.opt.dial(ctx)
		if err == nil {
			s.conn = c
			s.use(b, s.level, s.pw)
			if _, err = s.m.ReadSerial(ctx); err == nil {
				release = time.Since(t0)
				break
			}
			_ = c.Close()
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}

	v := vPass
	if secondServed || firstErr != nil || release == 0 {
		v = vFail
	}
	r.add("11", "второй клиент на порт", v, "второй: %s", second)
	if firstErr != nil {
		r.sub("первый клиент потерял связь: %v", firstErr)
	} else {
		r.sub("первый клиент продолжает обмен")
	}
	if release > 0 {
		r.sub("после штатного закрытия слот освободился через %s", release.Round(time.Millisecond))
	} else {
		r.sub("слот не освободился за 25 с: %v", lastErr)
	}
	r.sub("обрыв без FIN (выдернуть кабель хоста) проверяется вручную: ожидается освобождение ≤ 20 с")
}

// --- отдельные команды ---

func (b *bench) identity(ctx context.Context) error {
	return b.withSession(ctx, true, func(s *session) error {
		r := &report{out: b.out}
		b.checkIdentity(ctx, s, r)
		if r.failed() {
			return errors.New("identity check failed")
		}
		return nil
	})
}

func (b *bench) read(ctx context.Context) error {
	return b.withSession(ctx, true, func(s *session) error {
		id, err := s.m.ReadSerial(ctx)
		if err != nil {
			return err
		}
		b.printf("номер %s\n", id.Serial)
		if pw, err := s.m.ReadGroupPower(ctx, b.opt.bwri); err == nil {
			b.printf("08 16 %02X: P = %s; P1 %s, P2 %s, P3 %s\n", b.opt.bwri, watts(pw.TotalCW), watts(pw.PhaseCW[0]), watts(pw.PhaseCW[1]), watts(pw.PhaseCW[2]))
		} else if pw, perr := s.m.ReadPhasePower(ctx); perr == nil {
			b.printf("3 × 08 11: P1 %s, P2 %s, P3 %s (08 16: %v)\n", watts(pw.PhaseCW[0]), watts(pw.PhaseCW[1]), watts(pw.PhaseCW[2]), err)
		} else {
			b.printf("P: %v\n", perr)
		}
		if pe, _, err := s.m.ReadPhaseEnergy(ctx); err == nil {
			b.printf("05 60 00: AP1 %d, AP2 %d, AP3 %d Вт·ч\n", pe[0], pe[1], pe[2])
		} else {
			b.printf("05 60 00: %v\n", err)
		}
		te, _, err := s.m.ReadTotalEnergy(ctx)
		if err != nil {
			return err
		}
		b.printf("05 00 00: A+ %d Вт·ч, A− %s, R+ %s, R− %s\n", te.APlus, masked(te.AMinus, te.Masked[1]), masked(te.RPlus, te.Masked[2]), masked(te.RMinus, te.Masked[3]))
		return nil
	})
}

// expiry — проверка 12.
func (b *bench) expiry(ctx context.Context, wait time.Duration) error {
	return b.withSession(ctx, true, func(s *session) error {
		if _, _, err := s.m.ReadTotalEnergy(ctx); err != nil {
			return err
		}
		b.printf("канал открыт; ожидание %s без обмена (канал живёт 240 с)…\n", wait)
		deadline := time.Now().Add(wait)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				b.printf("… осталось %s\n", time.Until(deadline).Round(time.Second))
			case <-time.After(time.Until(deadline)):
			}
		}
		var seen []string
		saw5 := false
		b.onEx = func(ex mercury230.Exchange) {
			status := "ok"
			if ex.Err != nil {
				status = ex.Err.Error()
			}
			seen = append(seen, ex.Request+": "+status)
			saw5 = saw5 || mercury230.IsStatus(ex.Err, mercury230.StatusChannelClosed)
		}
		defer func() { b.onEx = nil }()
		te, _, err := s.m.ReadTotalEnergy(ctx)
		r := &report{out: b.out}
		switch {
		case err != nil:
			r.add("12", "истечение канала", vFail, "%v", err)
		case saw5:
			r.add("12", "истечение канала", vPass, "код 5 → 01h → повтор; A+ %d Вт·ч, снимок не потерян", te.APlus)
		default:
			r.add("12", "истечение канала", vWarn, "через %s канал ещё открыт (кода 5 нет)", wait)
		}
		for _, l := range seen {
			r.sub("%s", l)
		}
		if r.failed() {
			return errors.New("channel expiry check failed")
		}
		return nil
	})
}

func (b *bench) raw(ctx context.Context, n int, args []string) error {
	pdu, err := hex.DecodeString(strings.Join(args, ""))
	if err != nil || len(pdu) == 0 {
		return fmt.Errorf("raw: pdu must be hex bytes, e.g. 081601")
	}
	if n < mercury230.StatusLen {
		return fmt.Errorf("raw: -len must be >= %d (address + data + CRC)", mercury230.StatusLen)
	}
	return b.withSession(ctx, true, func(s *session) error {
		p, ex, err := s.m.Do(ctx, mercury230.Request{Name: "raw", PDU: pdu, RespLen: n, NeedsChannel: true})
		b.printf("→ % X\n← % X  (%s)\n", ex.Req, ex.Resp, ex.Duration.Round(100*time.Microsecond))
		if err != nil {
			return err
		}
		b.printf("данные: % X\n", p)
		return nil
	})
}

// --- форматирование ---

func date(t time.Time) string {
	if t.IsZero() {
		return "не разобрана"
	}
	return t.Format("02.01.2006")
}

func at(ds []time.Duration, q float64) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[min(len(s)-1, int(q*float64(len(s))))]
}

func pct(ds []time.Duration) string {
	if len(ds) == 0 {
		return "—"
	}
	r := func(d time.Duration) string { return d.Round(100 * time.Microsecond).String() }
	return r(at(ds, 0.5)) + "/" + r(at(ds, 0.99)) + "/" + r(slices.Max(ds))
}
