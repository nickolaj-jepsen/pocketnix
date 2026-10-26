// Command pocketnix-agent is the guest side of pocketnix: it serves the app's
// control, PTY, forward and font streams. SIGHUP reloads the config file.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/agent"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/transport"
)

var version = "dev"

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	var listen listFlag
	config := flag.String("config", "/etc/pocketnix/agent.json", "config file, as the NixOS module writes it")
	noPower := flag.Bool("no-power", false, "refuse the app's poweroff and reboot requests (desktop development)")
	flag.Var(&listen, "listen", "address to serve (vsock:<port>, tcp:<host>:<port>, unix:<path>); repeatable")
	stateDir := flag.String("state-dir", "", "directory for state that must outlive a crash (unfinished file-provider writes); empty keeps none")
	guestSocket := flag.String("guest-socket", "", "unix socket for guest programs (pocketnix-guest), e.g. "+protocol.GuestSocket+"; empty serves none")
	flag.Parse()
	if len(listen) == 0 {
		listen = listFlag{"vsock:" + strconv.Itoa(protocol.ControlPort)}
	}
	log.SetFlags(0)

	srv, err := agent.New(*config)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	srv.Version = version
	srv.StateDir = *stateDir
	srv.SweepTemps()
	if !*noPower {
		srv.Power = systemdPower{}
	}
	srv.Updates.Inhibit = agent.LogindInhibit

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := srv.Reload(); err != nil {
				log.Printf("reload: %v (keeping the previous config)", err)
			} else {
				log.Printf("config reloaded")
			}
		}
	}()

	go srv.Updates.Periodic(context.Background())
	go srv.WatchInhibitors(context.Background())
	if *guestSocket != "" {
		l, err := agent.ListenGuest(*guestSocket)
		if err != nil {
			log.Fatalf("guest socket: %v", err)
		}
		go func() { log.Fatalf("guest socket: %v", srv.ServeGuest(l)) }()
	}
	for _, addr := range listen {
		go serve(srv, addr)
	}
	log.Printf("pocketnix-agent %s (protocol %d) on %s", version, protocol.Version, listen.String())
	select {}
}

// serve retries a failing listen: the vsock transport can show up after the
// agent starts, and one bad address shouldn't take the others down.
func serve(srv *agent.Server, addr string) {
	for {
		l, err := transport.Listen(addr)
		if err != nil {
			log.Printf("listen on %s: %v (retrying)", addr, err)
			time.Sleep(2 * time.Second)
			continue
		}
		err = srv.Serve(l)
		l.Close()
		log.Printf("serve %s: %v (restarting)", addr, err)
		time.Sleep(time.Second)
	}
}

type systemdPower struct{}

func (systemdPower) Poweroff() error { return exec.Command("systemctl", "poweroff").Start() }
func (systemdPower) Reboot() error   { return exec.Command("systemctl", "reboot").Start() }
