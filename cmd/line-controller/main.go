// Command line-controller — сервис управления питанием линий EVSE: по посту из
// реестра запускается адаптер MR6C и контроллер со своей арендой.
//
// API (этап 4) пока нет: сервис поднимает посты, выполняет сверку и держит линии OFF.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/kmlebedev/EVcomm/internal/controller"
	"github.com/kmlebedev/EVcomm/internal/registry"
)

func main() {
	regPath := flag.String("registry", "/etc/evcomm/registry.yaml", "post registry")
	jitter := flag.Duration("start-jitter", 2*time.Second, "max random delay of the first connection per post")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	reg, err := registry.Load(*regPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "line-controller:", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	runCtx, stopRun := context.WithCancel(context.Background())

	stations := make([]*controller.Station, len(reg.Posts))
	var wg sync.WaitGroup
	for i, p := range reg.Posts {
		stations[i] = controller.NewStation(p, nil, *jitter, log)
		wg.Go(func() { stations[i].Run(runCtx) })
	}
	log.Info("started", "posts", len(stations))

	<-ctx.Done()
	log.Info("shutting down: switching all lines off")
	var off sync.WaitGroup
	for _, s := range stations {
		off.Go(func() {
			if err := s.ShutdownOff(context.Background(), 3*time.Second); err != nil {
				log.Error("OFF not confirmed on shutdown; MR6C safe mode will release outputs", "post", s.Post.ID, "err", err)
			}
		})
	}
	off.Wait()
	stopRun()
	wg.Wait()
}
