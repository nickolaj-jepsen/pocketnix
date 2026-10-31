// Command pocketnix-agent is the guest side of pocketnix: it serves the app's
// control, PTY, forward and font streams. SIGHUP reloads the config file.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/agent"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/portwatch"
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
	pruneMarker := flag.String("prune-marker", "", "file that marks this boot's prune done, somewhere the agent's restarts leave alone; empty prunes after every start")
	hostState := flag.String("host-state", "", "file to keep the phone's state in as JSON, which also drives the NixOS module's pocketnix-*.target units; empty does neither")
	// Hidden from -h: tests force the port watcher's /proc poller with it.
	pollPorts := flag.Bool("poll-ports", false, "")
	flag.Usage = func() {
		visible := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
		visible.SetOutput(flag.CommandLine.Output())
		flag.VisitAll(func(f *flag.Flag) {
			if f.Name != "poll-ports" {
				visible.Var(f.Value, f.Name, f.Usage)
			}
		})
		visible.Usage()
	}
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
	if *hostState != "" {
		srv.HostFile, srv.Systemctl = *hostState, "systemctl"
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
	go srv.Updates.RefreshPackages()
	// After the boot's own work. Only a guest's agent is root: as anyone else,
	// nix-store --gc still collects this machine's store through the daemon.
	if os.Geteuid() == 0 {
		go srv.Updates.PruneAfterBoot(context.Background(), 2*time.Minute, *pruneMarker)
	}
	go srv.WatchInhibitors(context.Background())
	if *pollPorts {
		srv.WatchPorts(nil)
	} else {
		srv.WatchPorts(loadEBPF)
	}
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

// ebpfHints are the kernel's TCP sockets entering or leaving LISTEN.
type ebpfHints struct{ *portwatch.EBPF }

func loadEBPF() (agent.PortHints, error) {
	w, err := portwatch.LoadEBPF()
	if err != nil {
		return nil, fmt.Errorf("eBPF: %w", err)
	}
	return ebpfHints{w}, nil
}

func (h ebpfHints) Wait() error {
	_, err := h.Read()
	return err
}

func (ebpfHints) String() string { return "eBPF on the inet_sock_set_state raw tracepoint" }
