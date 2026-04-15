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
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/VulpineOS/mobilebridge/pkg/mobilebridge"
)

func main() {
	var (
		device             = flag.String("device", "", "device serial (auto-pick if empty and exactly one is attached)")
		port               = flag.Int("port", 9222, "local TCP port for the CDP server")
		list               = flag.Bool("list", false, "list attached devices and exit")
		watch              = flag.Bool("watch", false, "continuously watch for device hotplug and log added/removed devices")
		health             = flag.Bool("health", false, "print device + connection state and exit")
		autoRestart        = flag.Bool("auto-restart", false, "if the upstream drops, auto-restart the bridge instead of exiting")
		devices            = flag.Bool("devices", false, "print enriched device list (Android version, SDK, RAM, battery) and exit")
		screenRecord       = flag.String("screenrecord", "", "start `adb screenrecord` on server start and pull to this path on shutdown")
		logcat             = flag.Bool("logcat", false, "after bridge start, print `adb logcat -d` filtered to Chrome processes")
		workerControl      = flag.String("worker-control", "", "run hosted worker control server on this addr instead of a single-device bridge")
		workerHeartbeatURL = flag.String("worker-heartbeat-url", "", "POST heartbeat payloads to this vulpine-api /v1/mobile/workers/heartbeat endpoint")
		workerID           = flag.String("worker-id", "", "stable worker id for hosted worker-control mode")
		workerToken        = flag.String("worker-token", "", "bearer token for hosted worker heartbeat requests")
		workerAdvertiseURL = flag.String("worker-advertise-url", "", "public or private URL that vulpine-api should use to reach this worker-control server")
		workerHostname     = flag.String("worker-hostname", "", "override hostname reported in worker heartbeats")
		workerInterval     = flag.Duration("worker-heartbeat-interval", 15*time.Second, "interval between hosted worker heartbeats")
		workerMaxSessions  = flag.Int("worker-max-sessions", 0, "maximum attached sessions this worker-control server should allow (0 = unlimited)")
	)
	flag.Parse()

	switch {
	case *list:
		if err := runList(); err != nil {
			log.Fatalf("list: %v", err)
		}
		return
	case *devices:
		if err := runDevicesEnriched(); err != nil {
			log.Fatalf("devices: %v", err)
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
	case *workerControl != "":
		if err := runWorkerControl(*workerControl, workerControlOptions{
			heartbeatURL: *workerHeartbeatURL,
			workerID:     *workerID,
			workerToken:  *workerToken,
			advertiseURL: *workerAdvertiseURL,
			hostname:     *workerHostname,
			interval:     *workerInterval,
			maxSessions:  *workerMaxSessions,
		}); err != nil {
			log.Fatalf("worker control: %v", err)
		}
		return
	}

	runBridge(*device, *port, *autoRestart, *screenRecord, *logcat)
}

func runBridge(device string, port int, autoRestart bool, screenRecord string, logcat bool) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	for attempt := 1; ; attempt++ {
		serial, err := resolveSerial(device)
		if err != nil {
			log.Fatalf("select device: %v", err)
		}
		log.Printf("using device %s (attempt %d)", serial, attempt)

		session, err := mobilebridge.StartAttachedServer(context.Background(), serial, fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			if autoRestart {
				log.Printf("start bridge failed: %v (retrying in 2s)", err)
				select {
				case <-sigCh:
					return
				case <-time.After(2 * time.Second):
					continue
				}
			}
			log.Fatalf("start bridge: %v", err)
		}
		log.Printf("mobilebridge listening on http://127.0.0.1:%d", port)

		if screenRecord != "" {
			if err := session.Proxy.StartScreenRecording(context.Background(), screenRecord); err != nil {
				log.Printf("screen record start failed: %v", err)
			} else {
				log.Printf("recording screen to %s (will pull on shutdown)", screenRecord)
			}
		}

		if logcat {
			go runLogcat(serial)
		}

		// Wait for either a signal (exit) or the proxy giving up on
		// reconnects. proxy.Done() is closed by Close() or when
		// reconnect() exhausts its backoff schedule — that's the one
		// signal we care about here, no polling required.
		select {
		case <-sigCh:
			log.Printf("shutting down")
			if screenRecord != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := session.Proxy.StopScreenRecording(ctx); err != nil {
					log.Printf("stop screen record: %v", err)
				} else {
					log.Printf("pulled screen recording to %s", screenRecord)
				}
				cancel()
			}
			_ = session.Close()
			return
		case <-session.Done():
			_ = session.Close()
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

type workerControlOptions struct {
	heartbeatURL string
	workerID     string
	workerToken  string
	advertiseURL string
	hostname     string
	interval     time.Duration
	maxSessions  int
}

func runWorkerControl(addr string, opts workerControlOptions) error {
	server := mobilebridge.NewWorkerControlServer(addr)
	server.SetMaxSessions(opts.maxSessions)
	if err := server.Start(); err != nil {
		return err
	}
	defer server.Stop()

	log.Printf("mobilebridge worker control listening on http://%s", addr)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if opts.heartbeatURL != "" {
		if opts.workerID == "" {
			return fmt.Errorf("worker id is required when worker-heartbeat-url is set")
		}
		advertiseURL := opts.advertiseURL
		if advertiseURL == "" {
			advertiseURL = "http://" + server.ListenAddr()
		}
		publisher := mobilebridge.NewWorkerHeartbeatPublisher(
			server,
			opts.heartbeatURL,
			opts.workerToken,
			opts.workerID,
			opts.hostname,
			advertiseURL,
			opts.interval,
		)
		go func() {
			if err := publisher.Run(ctx); err != nil {
				log.Printf("worker heartbeat stopped: %v", err)
			}
		}()
	}
	<-ctx.Done()
	return nil
}

func runList() error {
	devs, err := mobilebridge.ListDevices(context.Background())
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Println("no devices attached")
		return nil
	}
	for _, d := range devs {
		label := ""
		if info, err := mobilebridge.ChromeDevtoolsSocketInfo(context.Background(), d.Serial); err == nil {
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

	info, err := mobilebridge.ChromeDevtoolsSocketInfo(context.Background(), serial)
	if err != nil {
		fmt.Printf("socket:  NOT FOUND (%v)\n", err)
		return nil
	}
	fmt.Printf("socket:  %s (%s)\n", info.Name, info.Kind)
	return nil
}

func runDevicesEnriched() error {
	devs, err := mobilebridge.ListDevices(context.Background())
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Println("no devices attached")
		return nil
	}
	for i := range devs {
		d := &devs[i]
		if d.State == "device" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := d.Enrich(ctx); err != nil {
				log.Printf("enrich %s: %v", d.Serial, err)
			}
			cancel()
		}
		fmt.Printf("%-20s  %-12s  model=%s product=%s  android=%s sdk=%d ram=%dMB battery=%d%%\n",
			d.Serial, d.State, d.Model, d.Product,
			d.AndroidVersion, d.SDKLevel, d.RAM_MB, d.BatteryPercent)
	}
	return nil
}

// runLogcat runs `adb -s <serial> logcat -d` once and prints lines whose
// tag mentions Chrome or WebView. This is intentionally a one-shot tail
// (`-d` = dump and exit) so it can't leak an adb subprocess past bridge
// shutdown; re-run mobilebridge to collect a fresh snapshot.
func runLogcat(serial string) {
	cmd := exec.Command("adb", "-s", serial, "logcat", "-d")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("logcat: %v", err)
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "chromium") ||
			strings.Contains(line, "Chrome") ||
			strings.Contains(line, "WebView") {
			fmt.Println("[logcat]", line)
		}
	}
}

func resolveSerial(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	devs, err := mobilebridge.ListDevices(context.Background())
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
