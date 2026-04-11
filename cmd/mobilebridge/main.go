// Command mobilebridge runs a CDP-to-Android bridge.
//
// Usage:
//
//	mobilebridge --list
//	mobilebridge --port 9222
//	mobilebridge --device <serial> --port 9222
//	mobilebridge --watch
//	mobilebridge --health
//	mobilebridge --port 9222 --auto-restart
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/PopcornDev1/mobilebridge/pkg/mobilebridge"
)

func main() {
	var (
		device      = flag.String("device", "", "device serial (auto-pick if empty and exactly one is attached)")
		port        = flag.Int("port", 9222, "local TCP port for the CDP server")
		list        = flag.Bool("list", false, "list attached devices and exit")
		watch       = flag.Bool("watch", false, "continuously watch for device hotplug and log added/removed devices")
		health      = flag.Bool("health", false, "print device + connection state and exit")
		autoRestart = flag.Bool("auto-restart", false, "if the upstream drops, auto-restart the bridge instead of exiting")
	)
	flag.Parse()

	switch {
	case *list:
		if err := runList(); err != nil {
			log.Fatalf("list: %v", err)
		}
		return
	case *watch:
		if err := runWatch(); err != nil {
			log.Fatalf("watch: %v", err)
		}
		return
	case *health:
		if err := runHealth(*device); err != nil {
			log.Fatalf("health: %v", err)
		}
		return
	}

	runBridge(*device, *port, *autoRestart)
}

func runBridge(device string, port int, autoRestart bool) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	for attempt := 1; ; attempt++ {
		serial, err := resolveSerial(device)
		if err != nil {
			log.Fatalf("select device: %v", err)
		}
		log.Printf("using device %s (attempt %d)", serial, attempt)

		proxy, err := mobilebridge.NewProxy(serial, port)
		if err != nil {
			if autoRestart {
				log.Printf("new proxy failed: %v (retrying in 2s)", err)
				select {
				case <-sigCh:
					return
				case <-time.After(2 * time.Second):
					continue
				}
			}
			log.Fatalf("new proxy: %v", err)
		}

		srv := mobilebridge.NewServer(serial, fmt.Sprintf("127.0.0.1:%d", port))
		if err := srv.Start(); err != nil {
			_ = proxy.Close()
			log.Fatalf("start server: %v", err)
		}
		if err := srv.RunWithProxy(proxy); err != nil {
			_ = srv.Stop()
			_ = proxy.Close()
			log.Fatalf("wire proxy: %v", err)
		}
		log.Printf("mobilebridge listening on http://127.0.0.1:%d", port)

		// Wait for either a signal (exit) or the upstream dying (auto-restart).
		upstreamGone := make(chan struct{})
		go func() {
			ws := proxy.Upstream()
			if ws == nil {
				close(upstreamGone)
				return
			}
			// Block on Ping loop: reuse ReadMessage by polling CloseHandler
			// indirectly — a simple poll is enough since we only care about
			// "is the socket still alive".
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for range t.C {
				if proxy.Upstream() == nil {
					close(upstreamGone)
					return
				}
			}
		}()

		select {
		case <-sigCh:
			log.Printf("shutting down")
			_ = srv.Stop()
			_ = proxy.Close()
			return
		case <-upstreamGone:
			_ = srv.Stop()
			_ = proxy.Close()
			if !autoRestart {
				log.Printf("upstream dropped; exiting (pass --auto-restart to keep retrying)")
				return
			}
			log.Printf("upstream dropped; auto-restarting in 1s")
			select {
			case <-sigCh:
				return
			case <-time.After(1 * time.Second):
			}
		}
	}
}

func runList() error {
	devs, err := mobilebridge.ListDevices()
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Println("no devices attached")
		return nil
	}
	for _, d := range devs {
		label := ""
		if info, err := mobilebridge.ChromeDevtoolsSocketInfo(d.Serial); err == nil {
			switch info.Kind {
			case mobilebridge.SocketKindChrome:
				label = "[Chrome]"
			case mobilebridge.SocketKindWebView:
				label = "[WebView " + info.Name + "]"
			}
		}
		fmt.Printf("%-20s  %-12s  %s %s  %s\n", d.Serial, d.State, d.Model, d.Product, label)
	}
	return nil
}

func runWatch() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	events, err := mobilebridge.WatchDevices(ctx)
	if err != nil {
		return err
	}
	log.Printf("watching for device hotplug (ctrl-c to stop)")
	for ev := range events {
		log.Printf("%s: %s %s %s", ev.Type, ev.Device.Serial, ev.Device.State, ev.Device.Model)
	}
	return nil
}

func runHealth(device string) error {
	serial, err := resolveSerial(device)
	if err != nil {
		fmt.Printf("device:  NOT READY (%v)\n", err)
		return nil
	}
	fmt.Printf("device:  %s\n", serial)

	info, err := mobilebridge.ChromeDevtoolsSocketInfo(serial)
	if err != nil {
		fmt.Printf("socket:  NOT FOUND (%v)\n", err)
		return nil
	}
	fmt.Printf("socket:  %s (%s)\n", info.Name, info.Kind)
	return nil
}

func resolveSerial(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	devs, err := mobilebridge.ListDevices()
	if err != nil {
		return "", err
	}
	var ready []mobilebridge.Device
	for _, d := range devs {
		if d.State == "device" {
			ready = append(ready, d)
		}
	}
	switch len(ready) {
	case 0:
		return "", fmt.Errorf("no ready devices found (run `mobilebridge --list`)")
	case 1:
		return ready[0].Serial, nil
	default:
		return "", fmt.Errorf("multiple devices attached; pass --device <serial>")
	}
}
