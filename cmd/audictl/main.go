package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"audictl/internal/mpv"
	"audictl/internal/provider"
	sprov "audictl/providers/spotify"
	yprov "audictl/providers/youtube"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: audictl <command> [args]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	switch cmd {
	case "play":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: audictl play <query|uri>")
			os.Exit(2)
		}
		query := strings.Join(os.Args[2:], " ")
		// if daemon socket exists, send request; otherwise run one-shot
		if socketExists() {
			req := map[string]interface{}{"cmd": "play", "args": map[string]string{"query": query}}
			if err := sendRPC(req); err != nil {
				fmt.Fprintf(os.Stderr, "daemon RPC error: %v\n", err)
				os.Exit(1)
			}
			return
		}
		runPlay(query)
	case "daemon":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: audictl daemon start|stop|status")
			os.Exit(2)
		}
		sub := os.Args[2]
		switch sub {
		case "start":
			// launch background daemon using separate binary if present
			// if cmd/audictld binary exists in same dir, exec it; otherwise run in-process
			exePath := os.Args[0]
			exeDir := filepath.Dir(exePath)
			daemonPath := filepath.Join(exeDir, "audictld")
			if _, err := os.Stat(daemonPath); err == nil {
				// exec external daemon
				cmd := exec.Command(daemonPath)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Start(); err != nil {
					fmt.Fprintf(os.Stderr, "failed to start external audictld: %v\n", err)
					os.Exit(1)
				}
				fmt.Printf("started audictld (pid=%d)\n", cmd.Process.Pid)
				return
			}
			fmt.Println("starting audictld (foreground)")
			runDaemon()
		default:
			fmt.Fprintf(os.Stderr, "unknown daemon command: %s\n", sub)
			os.Exit(2)
		}
	case "queue.add":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: audictl queue.add <query>")
			os.Exit(2)
		}
		q := strings.Join(os.Args[2:], " ")
		req := map[string]interface{}{"cmd": "queue.add", "args": map[string]string{"query": q}}
		if err := sendRPC(req); err != nil {
			fmt.Fprintf(os.Stderr, "rpc error: %v\n", err)
			os.Exit(1)
		}
	case "queue.list":
		req := map[string]interface{}{"cmd": "queue.list"}
		if err := sendRPC(req); err != nil {
			fmt.Fprintf(os.Stderr, "rpc error: %v\n", err)
			os.Exit(1)
		}
	case "stop":
		req := map[string]interface{}{"cmd": "stop"}
		if err := sendRPC(req); err != nil {
			fmt.Fprintf(os.Stderr, "rpc error: %v\n", err)
			os.Exit(1)
		}
	case "next":
		req := map[string]interface{}{"cmd": "next"}
		if err := sendRPC(req); err != nil {
			fmt.Fprintf(os.Stderr, "rpc error: %v\n", err)
			os.Exit(1)
		}
	case "status":
		req := map[string]interface{}{"cmd": "status"}
		if err := sendRPC(req); err != nil {
			fmt.Fprintf(os.Stderr, "rpc error: %v\n", err)
			os.Exit(1)
		}
	case "shell":
		// start interactive REPL shell
		runShell()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		os.Exit(2)
	}
}

func socketPath() string {
	sock := os.Getenv("AUDICTL_SOCKET")
	if sock != "" {
		return sock
	}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "audictl.sock")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "run", "audictl.sock")
}

func socketExists() bool {
	_, err := os.Stat(socketPath())
	return err == nil
}

func sendRPC(req map[string]interface{}) error {
	sock := socketPath()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return err
	}
	// read response
	dec := json.NewDecoder(conn)
	var resp map[string]interface{}
	if err := dec.Decode(&resp); err != nil {
		return err
	}
	pretty, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Println(string(pretty))
	return nil
}

func runPlay(query string) {
	// simple heuristic: if starts with spotify: use spotify metadata then search youtube
	var prov provider.Provider
	var track provider.Track
	if strings.HasPrefix(query, "spotify:") {
		sp := sprov.New()
		prov = sp
		t, err := sp.GetTrack(query)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spotify lookup failed: %v\n", err)
			os.Exit(1)
		}
		track = t
		// search youtube by artist - title
		yt := yprov.New()
		q := track.Artist + " - " + track.Title
		results, err := yt.Search(q, provider.SearchKindTrack, 1)
		if err != nil || len(results) == 0 {
			fmt.Fprintf(os.Stderr, "youtube search failed: %v\n", err)
			os.Exit(1)
		}
		track = results[0]
		prov = yt
	} else {
		// try youtube directly
		yt := yprov.New()
		results, err := yt.Search(query, provider.SearchKindTrack, 1)
		if err != nil || len(results) == 0 {
			fmt.Fprintf(os.Stderr, "search failed: %v\n", err)
			os.Exit(1)
		}
		track = results[0]
		prov = yt
	}

	fmt.Printf("Playing: %s - %s\n", track.Artist, track.Title)
	stream, err := prov.ResolveStream(track, provider.QualityAny)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve stream failed: %v\n", err)
		os.Exit(1)
	}

	device := os.Getenv("AUDICTL_DEVICE") // optional
	resample := false
	if os.Getenv("AUDICTL_RESAMPLE") == "1" {
		resample = true
	}

	if stream.URL == "" {
		fmt.Fprintln(os.Stderr, "no playable stream URL")
		os.Exit(1)
	}

	// For one-shot CLI runs, capture mpv output to present a helpful error.
	out, err := mpv.RunCapture(stream.URL, track.Title, device, resample)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mpv failed:")
		fmt.Fprintln(os.Stderr, out)
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runDaemon() {
	// very small foreground daemon placeholder: intercept SIGINT and SIGTERM and sleep
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	fmt.Println("audictld running (Ctrl+C to stop)")
	<-sigs
	fmt.Println("audictld stopping")
}
