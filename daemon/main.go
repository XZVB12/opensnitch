package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	golog "log"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"sync"
	"syscall"

	"github.com/gustavo-iniguez-goya/opensnitch/daemon/conman"
	"github.com/gustavo-iniguez-goya/opensnitch/daemon/core"
	"github.com/gustavo-iniguez-goya/opensnitch/daemon/dns"
	"github.com/gustavo-iniguez-goya/opensnitch/daemon/firewall"
	"github.com/gustavo-iniguez-goya/opensnitch/daemon/log"
	"github.com/gustavo-iniguez-goya/opensnitch/daemon/netfilter"
	"github.com/gustavo-iniguez-goya/opensnitch/daemon/procmon"
	"github.com/gustavo-iniguez-goya/opensnitch/daemon/rule"
	"github.com/gustavo-iniguez-goya/opensnitch/daemon/statistics"
	"github.com/gustavo-iniguez-goya/opensnitch/daemon/ui"
)

var (
	lock          sync.RWMutex
	procmonMethod = ""
	logFile       = ""
	rulesPath     = "rules"
	noLiveReload  = false
	queueNum      = 0
	workers       = 16
	debug         = false
	warning       = false
	important     = false
	errorlog      = false

	uiSocket = "unix:///tmp/osui.sock"
	uiClient = (*ui.Client)(nil)

	cpuProfile = ""
	memProfile = ""

	err     = (error)(nil)
	rules   = (*rule.Loader)(nil)
	stats   = (*statistics.Statistics)(nil)
	queue   = (*netfilter.Queue)(nil)
	pktChan = (<-chan netfilter.Packet)(nil)
	wrkChan = (chan netfilter.Packet)(nil)
	sigChan = (chan os.Signal)(nil)
)

func init() {
	flag.StringVar(&procmonMethod, "process-monitor-method", procmonMethod, "How to search for processes path. Options: ftrace, audit (experimental), proc (default)")
	flag.StringVar(&uiSocket, "ui-socket", uiSocket, "Path the UI gRPC service listener (https://github.com/grpc/grpc/blob/master/doc/naming.md).")
	flag.StringVar(&rulesPath, "rules-path", rulesPath, "Path to load JSON rules from.")
	flag.IntVar(&queueNum, "queue-num", queueNum, "Netfilter queue number.")
	flag.IntVar(&workers, "workers", workers, "Number of concurrent workers.")
	flag.BoolVar(&noLiveReload, "no-live-reload", debug, "Disable rules live reloading.")

	flag.StringVar(&logFile, "log-file", logFile, "Write logs to this file instead of the standard output.")
	flag.BoolVar(&debug, "debug", debug, "Enable debug logs.")
	flag.BoolVar(&warning, "warning", warning, "Enable warning logs.")
	flag.BoolVar(&important, "important", important, "Enable important level logs.")
	flag.BoolVar(&errorlog, "error", errorlog, "Enable error level logs.")

	flag.StringVar(&cpuProfile, "cpu-profile", cpuProfile, "Write CPU profile to this file.")
	flag.StringVar(&memProfile, "mem-profile", memProfile, "Write memory profile to this file.")
}

func setupLogging() {
	golog.SetOutput(ioutil.Discard)
	if debug {
		log.SetLogLevel(log.DEBUG)
	} else if warning {
		log.SetLogLevel(log.WARNING)
	} else if important {
		log.SetLogLevel(log.IMPORTANT)
	} else if errorlog {
		log.SetLogLevel(log.ERROR)
	} else {
		log.SetLogLevel(log.INFO)
	}

	if logFile != "" {
		if log.Output, err = os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err != nil {
			panic(err)
		}
	}
}

func setupSignals() {
	sigChan = make(chan os.Signal, 1)
	signal.Notify(sigChan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		sig := <-sigChan
		log.Raw("\n")
		log.Important("Got signal: %v", sig)
		doCleanup()
		os.Exit(0)
	}()
}

func worker(id int) {
	log.Debug("Worker #%d started.", id)
	for true {
		select {
		case pkt := <-wrkChan:
			onPacket(pkt)
		}
	}
}

func setupWorkers() {
	log.Debug("Starting %d workers ...", workers)
	// setup the workers
	wrkChan = make(chan netfilter.Packet)
	for i := 0; i < workers; i++ {
		go worker(i)
	}
}

func doCleanup() {
	log.Info("Cleaning up ...")
	firewall.Stop(&queueNum)
	log.Info("Cleaning up firewall...")
	procmon.End()

	if cpuProfile != "" {
		pprof.StopCPUProfile()
	}

	if memProfile != "" {
		f, err := os.Create(memProfile)
		if err != nil {
			fmt.Printf("Could not create memory profile: %s\n", err)
			return
		}
		defer f.Close()
		runtime.GC() // get up-to-date statistics
		if err := pprof.WriteHeapProfile(f); err != nil {
			fmt.Printf("Could not write memory profile: %s\n", err)
		}
	}
}

func onPacket(packet netfilter.Packet) {
	// DNS response, just parse, track and accept.
	if dns.TrackAnswers(packet.Packet) == true {
		packet.SetVerdict(netfilter.NF_ACCEPT)
		stats.OnDNSResponse()
		return
	}

	// Parse the connection state
	con := conman.Parse(packet, uiClient.InterceptUnknown())
	if con == nil {
		applyDefaultAction(&packet)
		return
	}
	// accept our own connections
	if con.Process.ID == os.Getpid() {
		packet.SetVerdict(netfilter.NF_ACCEPT)
		return
	}

	// search a match in preloaded rules
	r := acceptOrDeny(&packet, con)

	stats.OnConnectionEvent(con, r, r == nil)
}

func applyDefaultAction(packet *netfilter.Packet) {
	if uiClient.DefaultAction() == rule.Allow {
		packet.SetVerdict(netfilter.NF_ACCEPT)
	} else {
		if uiClient.DefaultDuration() == rule.Always {
			packet.SetVerdictAndMark(netfilter.NF_DROP, firewall.DropMark)
		} else {
			packet.SetVerdict(netfilter.NF_DROP)
		}
	}
}

func acceptOrDeny(packet *netfilter.Packet, con *conman.Connection) *rule.Rule {
	lock.Lock()
	defer lock.Unlock()

	connected := false
	r := rules.FindFirstMatch(con)
	if r == nil {
		// no rule matched, send a request to the
		// UI client if connected and running
		r, connected = uiClient.Ask(con)
		if r == nil {
			log.Error("Invalid rule received, applying default action")
			applyDefaultAction(packet)
			return nil
		}
		if connected {
			ok := false
			pers := ""
			action := string(r.Action)
			if r.Action == rule.Allow {
				action = log.Green(action)
			} else {
				action = log.Red(action)
			}

			// check if and how the rule needs to be saved
			if r.Duration == rule.Always {
				pers = "Saved"
				// add to the loaded rules and persist on disk
				if err := rules.Add(r, true); err != nil {
					log.Error("Error while saving rule: %s", err)
				} else {
					ok = true
				}
			} else {
				pers = "Added"
				// add to the rules but do not save to disk
				if err := rules.Add(r, false); err != nil {
					log.Error("Error while adding rule: %s", err)
				} else {
					ok = true
				}
			}

			if ok {
				log.Important("%s new rule: %s if %s", pers, action, r.Operator.String())
			}
		}
	}

	if r.Enabled == false {
		applyDefaultAction(packet)
		ruleName := log.Green(r.Name)
		log.Info("DISABLED (%s) %s %s -> %s:%d (%s)", uiClient.DefaultAction(), log.Bold(log.Green("✔")), log.Bold(con.Process.Path), log.Bold(con.To()), con.DstPort, ruleName)

	} else if r.Action == rule.Allow {
		if packet != nil {
			packet.SetVerdict(netfilter.NF_ACCEPT)
		}

		ruleName := log.Green(r.Name)
		if r.Operator.Operand == rule.OpTrue {
			ruleName = log.Dim(r.Name)
		}
		log.Debug("%s %s -> %s:%d (%s)", log.Bold(log.Green("✔")), log.Bold(con.Process.Path), log.Bold(con.To()), con.DstPort, ruleName)
	} else {
		if packet != nil {
			packet.SetVerdictAndMark(netfilter.NF_DROP, firewall.DropMark)
		}

		log.Debug("%s %s -> %s:%d (%s)", log.Bold(log.Red("✘")), log.Bold(con.Process.Path), log.Bold(con.To()), con.DstPort, log.Red(r.Name))
	}

	return r
}

func main() {
	flag.Parse()

	setupLogging()

	if cpuProfile != "" {
		if f, err := os.Create(cpuProfile); err != nil {
			log.Fatal("%s", err)
		} else if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("%s", err)
		}
	}

	log.Important("Starting %s v%s", core.Name, core.Version)

	rulesPath, err := core.ExpandPath(rulesPath)
	if err != nil {
		log.Fatal("%s", err)
	}

	setupSignals()

	log.Info("Loading rules from %s ...", rulesPath)
	if rules, err = rule.NewLoader(!noLiveReload); err != nil {
		log.Fatal("%s", err)
	} else if err = rules.Load(rulesPath); err != nil {
		log.Fatal("%s", err)
	}
	stats = statistics.New(rules)

	// prepare the queue
	setupWorkers()
	queue, err := netfilter.NewQueue(uint16(queueNum))
	if err != nil {
		log.Warning("Is opnensitchd already running?")
		log.Fatal("Error while creating queue #%d: %s", queueNum, err)
	}
	pktChan = queue.Packets()

	// clean any possible residual firewall rule
	firewall.QueueDNSResponses(false, queueNum)
	firewall.QueueConnections(false, queueNum)
	firewall.DropMarked(false)

	uiClient = ui.NewClient(uiSocket, stats, rules)
	// overwrite monitor method from configuration if the user has passed
	// the option via command line.
	if procmonMethod != "" {
		procmon.SetMonitorMethod(procmonMethod)
	}
	procmon.Init()

	// queue is ready, run firewall rules
	firewall.Init(&queueNum)

	log.Info("Running on netfilter queue #%d ...", queueNum)
	for true {
		select {
		case pkt := <-pktChan:
			wrkChan <- pkt
		}
	}
}
