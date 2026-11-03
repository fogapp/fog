// fog-node lends a machine's hardware to fog (docs/compute-contract.md):
// paired once with a code from Settings › Compute, it reports what the
// machine can do and runs the work fog hands it, within the budget its
// owner sets.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gitea.lan/matthew/fog/internal/computenode"
	"gitea.lan/matthew/fog/internal/config"
)

func usage() {
	fmt.Fprint(os.Stderr, `fog-node — lend this machine's hardware to fog

usage: fog-node <command> [flags]

commands:
  join      pair with fog: --fog http://fog-address:8080 [--name NAME], the code
            from FOG_PAIR_CODE, stdin, or asked for (never an argument)
  install   run fog-node at login (launchd) or boot (systemd), from this binary
  uninstall stop running fog-node and remove the service
  run       poll fog and do its work (what the service runs)
  probe     print what this machine can do
  busy      on|off — take this machine back from batch work, or lend it again
  models    off | URL — stop lending the model server, or lend the one at URL
            (default: think, then Ollama, on this machine, when one answers)
  unconfined on|off — run fog's ffmpeg without confinement (UNSAFE: only for a
            machine that can't confine it; fog shows that you chose this)
  status    print the pairing

every command takes --dir (default: `+computenode.DefaultDir()+`)
`)
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == computenode.ConfineArg {
		// A run's ffmpeg starting confined: this returns only when it can't.
		err := computenode.ConfineMain(os.Args[2:])
		fmt.Fprintln(os.Stderr, "fog-node: not running ffmpeg, it couldn't be confined:", err)
		os.Exit(125)
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dir := fs.String("dir", computenode.DefaultDir(), "where fog-node keeps its pairing")
	fog := fs.String("fog", "", "fog's address")
	code := fs.String("code", "", "refused: the pairing code goes in FOG_PAIR_CODE or on stdin, never on the command line")
	name := fs.String("name", "", "what fog calls this machine (default: its hostname)")
	ffmpeg := fs.String("ffmpeg", "", "the ffmpeg to use (default: the first found)")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "join":
		if *code != "" {
			fmt.Fprintln(os.Stderr, "fog-node join: don't put the pairing code on the command line (anyone on this machine can read it there); "+
				"set FOG_PAIR_CODE, or pipe it in, or type it when asked")
			os.Exit(2)
		}
		if *fog == "" {
			fmt.Fprintln(os.Stderr, "fog-node join needs --fog")
			os.Exit(2)
		}
		pairCode, cerr := pairingCode(os.Stdin)
		if cerr != nil {
			fmt.Fprintln(os.Stderr, "fog-node join:", cerr)
			os.Exit(2)
		}
		var cfg *computenode.Config
		cfg, err = computenode.Join(ctx, *dir, *fog, pairCode, *name, *ffmpeg, config.Version)
		if err == nil {
			fmt.Printf("paired with fog at %s as %q\n", cfg.Fog, cfg.Name)
		}
	case "install":
		err = service(*dir).Install()
		if err == nil {
			fmt.Println("fog-node runs as", service(*dir).Path())
		}
	case "uninstall":
		err = service(*dir).Uninstall()
	case "run":
		err = run(ctx, *dir, log)
	case "probe":
		inv := computenode.Probe(ctx, *name, computenode.FindFFmpeg(*ffmpeg), config.Version)
		unconfined := false
		if cfg, err := computenode.LoadConfig(*dir); err == nil {
			unconfined = cfg.Unconfined
		}
		runner(*dir, computenode.FindFFmpeg(*ffmpeg), unconfined, log).Video(ctx, inv.Video)
		out, _ := json.MarshalIndent(inv, "", "  ")
		fmt.Println(string(out))
	case "busy":
		err = setBusy(*dir, fs.Arg(0))
	case "models":
		err = setModels(*dir, fs.Arg(0))
	case "unconfined":
		err = setUnconfined(*dir, fs.Arg(0))
	case "status":
		var cfg *computenode.Config
		cfg, err = computenode.LoadConfig(*dir)
		if err == nil {
			fmt.Printf("paired with fog at %s as %q (node %s)%s\n", cfg.Fog, cfg.Name, cfg.Node, map[bool]string{true: ", busy"}[cfg.Busy])
		}
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "fog-node:", err)
		os.Exit(1)
	}
}

func service(dir string) computenode.Service {
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return computenode.Service{Exe: exe, Dir: dir}
}

// runner is what runs fog's ffmpeg here: each run's scratch directory
// under the node's own, confined (or, by its owner's choice, not).
func runner(dir, ffmpeg string, unconfined bool, log *slog.Logger) *computenode.Runner {
	return &computenode.Runner{FFmpeg: ffmpeg, Work: filepath.Join(dir, "work"), Unconfined: unconfined, Log: log}
}

// unpairedWait is how often a machine fog removed (or never paired) looks
// for a new pairing: the service stays up, idle, rather than failing over
// and over, and starts lending again once fog-node join pairs it.
const unpairedWait = 10 * time.Second

func run(ctx context.Context, dir string, log *slog.Logger) error {
	said := false
	for ctx.Err() == nil {
		cfg, err := computenode.LoadConfig(dir)
		if errors.Is(err, computenode.ErrNotPaired) {
			if !said {
				log.Warn("lending nothing", "why", err.Error(), "pair", "fog-node join, with a code from fog's Settings › Compute")
				said = true
			}
			select {
			case <-ctx.Done():
			case <-time.After(unpairedWait):
			}
			continue
		}
		if err != nil {
			return err
		}
		said = false
		if err := runPaired(ctx, dir, cfg, log); !errors.Is(err, computenode.ErrUnpaired) {
			return err
		}
	}
	return ctx.Err()
}

// runPaired lends the machine to the fog it's paired with, until ctx ends
// or that fog removes it: then its runs are stopped, its listener closed,
// and the pairing forgotten.
func runPaired(ctx context.Context, dir string, cfg *computenode.Config, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := runner(dir, computenode.FindFFmpeg(cfg.FFmpeg), cfg.Unconfined, log)
	work.Fog = cfg.Fog
	work.Auth = func() (string, string) { return cfg.Node, cfg.Secret() }
	if err := os.RemoveAll(work.Work); err != nil {
		return err
	}
	if cfg.Unconfined {
		log.Warn("UNCONFINED: fog's ffmpeg runs with all of this user's access, because this machine's owner turned " +
			"confinement off (fog-node unconfined off turns it back on). fog's Settings › Compute shows it.")
	}
	lender := &computenode.Lender{Config: cfg, Log: log}
	go lender.Run(ctx)
	a := &computenode.Agent{Dir: dir, Config: cfg, Version: config.Version, Log: log, Work: work, Lending: lender,
		Exe: service(dir).Exe, Exit: os.Exit}
	log.Info("fog-node running", "fog", cfg.Fog, "name", cfg.Name, "version", config.Version)
	return a.Run(ctx)
}

func setUnconfined(dir, arg string) error {
	cfg, err := computenode.LoadConfig(dir)
	if err != nil {
		return err
	}
	switch arg {
	case "on":
		cfg.Unconfined = true
	case "off":
		cfg.Unconfined = false
	default:
		return errors.New("fog-node unconfined takes on or off")
	}
	if err := cfg.Save(dir); err != nil {
		return err
	}
	if cfg.Unconfined {
		fmt.Fprintln(os.Stderr, `WARNING: fog's ffmpeg will run on this machine WITHOUT confinement.
The command lines it runs come from fog on behalf of apps. Unconfined, one
that gets past fog-node's checks, or a malicious video, can read and change
anything this user can (your files, SSH keys, fog-node's own credential)
and reach anything on your network. fog's Settings › Compute shows this
machine as unconfined. Turn it back off with: fog-node unconfined off`)
	} else {
		fmt.Println("fog's ffmpeg runs confined again (or this machine lends no video, if it can't confine it)")
	}
	fmt.Println("saved; restart fog-node (fog-node install) for it to take effect")
	return nil
}

func setBusy(dir, arg string) error {
	cfg, err := computenode.LoadConfig(dir)
	if err != nil {
		return err
	}
	switch arg {
	case "on":
		cfg.Busy = true
	case "off":
		cfg.Busy = false
	default:
		return errors.New("fog-node busy takes on or off")
	}
	// The running agent reads it on its next poll.
	if err := cfg.Save(dir); err != nil {
		return err
	}
	if cfg.Busy {
		fmt.Println("busy: this machine takes no batch work until fog-node busy off")
	} else {
		fmt.Println("lending this machine for batch work again")
	}
	return nil
}

func setModels(dir, arg string) error {
	cfg, err := computenode.LoadConfig(dir)
	if err != nil {
		return err
	}
	switch {
	case arg == "off":
		cfg.InferenceOff, cfg.InferenceUpstream = true, ""
	case strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://"):
		cfg.InferenceOff, cfg.InferenceUpstream = false, arg
	case arg == "" || arg == "on":
		cfg.InferenceOff, cfg.InferenceUpstream = false, ""
	default:
		return errors.New("fog-node models takes off, on, or the model server's URL")
	}
	if err := cfg.Save(dir); err != nil {
		return err
	}
	fmt.Println("saved; restart fog-node (fog-node install) for it to take effect")
	return nil
}

// pairCodeEnv is where the pairing code comes from: never an argument, which
// any user of the machine could read.
const pairCodeEnv = "FOG_PAIR_CODE"

// pairingCode reads the pairing code from FOG_PAIR_CODE, else from stdin
// (piped, or typed when asked), and takes it out of the environment so
// nothing this process starts inherits it.
func pairingCode(stdin *os.File) (string, error) {
	code := strings.TrimSpace(os.Getenv(pairCodeEnv))
	os.Unsetenv(pairCodeEnv)
	if code != "" {
		return code, nil
	}
	if info, err := stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "Pairing code (from fog's Settings › Compute): ")
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if code = strings.TrimSpace(line); code == "" {
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return "", errors.New("no pairing code: set " + pairCodeEnv + " or give it on stdin")
	}
	return code, nil
}
