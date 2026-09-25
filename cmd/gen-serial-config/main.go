// Command gen-serial-config генерирует /etc/wb-mqtt-serial.conf из реестра постов:
// порт tcp (IP шлюза, 503) на каждый пост, счётчики с явным id вида post017_meter.
//
// Этап 4 дорожной карты; пока не реализовано.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gen-serial-config: not implemented yet (roadmap stage 4)")
	os.Exit(2)
}
