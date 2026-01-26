package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"audictl/internal/mpv"
	"audictl/internal/provider"
	sprov "audictl/providers/spotify"
	yprov "audictl/providers/youtube"
)

const socketPathEnv = "AUDICTL_SOCKET"

type request struct {
	Cmd  string            `json:"cmd"`
	Args map[string]string `json:"args,omitempty"`
}

type response struct {
	Ok     bool        `json:"ok"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

type daemon struct {
	mu         sync.Mutex
	queue      []provider.Track
	curr       *provider.Track
	currCmd    *exec.Cmd
	currWaitCh chan error
	providers  map[string]provider.Provider
	listener   net.Listener
}

func main() {
	// socket path
	sock := os.Getenv(socketPathEnv)
	if sock == "" {
		sock = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "audictl.sock")
		if os.Getenv("XDG_RUNTIME_DIR") == "" {
			// fallback to ~/.local/run/audictl.sock
			home, _ := os.UserHomeDir()
			sock = filepath.Join(home, ".local", "run", "audictl.sock")
		}
	}

	// ensure directory exists
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create socket dir: %v\n", err)
		os.Exit(1)
	}
	// remove stale socket
	_ = os.Remove(sock)

	l, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen on socket %s: %v\n", sock, err)
		os.Exit(1)
	}
	defer l.Close()

	d := &daemon{
		queue:     []provider.Track{},
		providers: map[string]provider.Provider{},
	}
	d.providers["youtube"] = yprov.New()
	d.providers["spotify"] = sprov.New()
	d.listener = l

	// handle signals
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Println("shutting down audictld")
		l.Close()
		os.Exit(0)
	}()

	fmt.Printf("audictld listening on %s\n", sock)
	for {
		conn, err := l.Accept()
		if err != nil {
			// listener closed
			return
		}
		go d.handleConn(conn)
	}
}

func (d *daemon) handleConn(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	var req request
	dec := json.NewDecoder(r)
	if err := dec.Decode(&req); err != nil {
		d.writeResp(c, response{Ok: false, Error: err.Error()})
		return
	}
	switch req.Cmd {
	case "play":
		q := req.Args["query"]
		if q == "" {
			d.writeResp(c, response{Ok: false, Error: "missing query"})
			return
		}
		if err := d.enqueueAndPlay(q, true); err != nil {
			d.writeResp(c, response{Ok: false, Error: err.Error()})
			return
		}
		d.writeResp(c, response{Ok: true, Result: "playing"})
	case "queue.add":
		q := req.Args["query"]
		if q == "" {
			d.writeResp(c, response{Ok: false, Error: "missing query"})
			return
		}
		if err := d.enqueue(q); err != nil {
			d.writeResp(c, response{Ok: false, Error: err.Error()})
			return
		}
		d.writeResp(c, response{Ok: true, Result: "queued"})
	case "queue.list":
		d.mu.Lock()
		q := d.queue
		d.mu.Unlock()
		d.writeResp(c, response{Ok: true, Result: q})
	case "stop":
		if err := d.stopPlayback(); err != nil {
			d.writeResp(c, response{Ok: false, Error: err.Error()})
			return
		}
		d.writeResp(c, response{Ok: true, Result: "stopped"})
	case "next":
		if err := d.next(); err != nil {
			d.writeResp(c, response{Ok: false, Error: err.Error()})
			return
		}
		d.writeResp(c, response{Ok: true, Result: "ok"})
	case "status":
		d.mu.Lock()
		var curr *provider.Track
		if d.curr != nil {
			cpy := *d.curr
			curr = &cpy
		}
		q := d.queue
		d.mu.Unlock()
		d.writeResp(c, response{Ok: true, Result: map[string]interface{}{"current": curr, "queue": q}})
	default:
		d.writeResp(c, response{Ok: false, Error: "unknown command"})
	}
}

func (d *daemon) writeResp(c net.Conn, resp response) {
	enc := json.NewEncoder(c)
	_ = enc.Encode(resp)
}

func (d *daemon) enqueue(query string) error {
	// resolve via providers: if starts with spotify:, use spotify metadata then search youtube; else prefer youtube
	var t provider.Track
	var err error
	if len(query) >= 8 && query[:8] == "spotify:" {
		sp := d.providers["spotify"]
		t, err = sp.GetTrack(query)
		if err != nil {
			return err
		}
		// search youtube by artist - title
		yt := d.providers["youtube"]
		q := t.Artist + " - " + t.Title
		res, err := yt.Search(q, provider.SearchKindTrack, 1)
		if err != nil || len(res) == 0 {
			return fmt.Errorf("youtube search failed: %w", err)
		}
		t = res[0]
	} else {
		yt := d.providers["youtube"]
		res, err := yt.Search(query, provider.SearchKindTrack, 1)
		if err != nil || len(res) == 0 {
			return fmt.Errorf("search failed: %w", err)
		}
		t = res[0]
	}
	d.mu.Lock()
	d.queue = append(d.queue, t)
	d.mu.Unlock()
	return nil
}

func (d *daemon) enqueueAndPlay(query string, immediate bool) error {
	if err := d.enqueue(query); err != nil {
		return err
	}
	if immediate {
		// if nothing playing, start next
		d.mu.Lock()
		playing := d.curr != nil
		d.mu.Unlock()
		if !playing {
			return d.next()
		}
	}
	return nil
}

func (d *daemon) resolveNext() (*provider.Track, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) == 0 {
		return nil, nil
	}
	t := d.queue[0]
	d.queue = d.queue[1:]
	d.curr = &t
	return &t, nil
}

func (d *daemon) next() error {
	// stop current
	_ = d.stopPlayback()
	t, err := d.resolveNext()
	if err != nil {
		return err
	}
	if t == nil {
		return nil
	}
	// resolve stream
	yt := d.providers["youtube"]
	stream, err := yt.ResolveStream(*t, provider.QualityAny)
	if err != nil {
		return err
	}
	// start mpv
	device := os.Getenv("AUDICTL_DEVICE")
	resample := os.Getenv("AUDICTL_RESAMPLE") == "1"
	cmd, err := mpv.Start(stream.URL, t.Title, device, resample)
	if err != nil {
		return err
	}
	// track process + enable auto-advance only if the same process finishes
	ch := make(chan error, 1)
	d.mu.Lock()
	d.currCmd = cmd
	d.currWaitCh = ch
	d.mu.Unlock()

	go func(c *exec.Cmd, done chan error) {
		err := c.Wait()
		// signal wait result (non-blocking due to buffered chan)
		select {
		case done <- err:
		default:
		}
		// only auto-advance if this is still the current command
		d.mu.Lock()
		same := d.currCmd == c
		if same {
			// clear current before advancing
			d.curr = nil
			d.currCmd = nil
			d.currWaitCh = nil
		}
		d.mu.Unlock()
		if same {
			_ = d.next()
		}
	}(cmd, ch)
	return nil
}

func (d *daemon) stopPlayback() error {
	d.mu.Lock()
	cmd := d.currCmd
	ch := d.currWaitCh
	// clear state immediately to avoid races
	d.currCmd = nil
	d.currWaitCh = nil
	d.curr = nil
	d.mu.Unlock()

	if cmd == nil {
		return nil
	}

	// attempt graceful kill
	if err := mpv.KillCmd(cmd); err != nil {
		// still continue to wait for process exit so we don't leak
		fmt.Fprintf(os.Stderr, "mpv kill error: %v\n", err)
	}

	// wait for the process to exit, prefer using the daemon's wait channel if present
	if ch != nil {
		select {
		case err := <-ch:
			if err != nil {
				return fmt.Errorf("mpv exit error: %w", err)
			}
			return nil
		case <-time.After(3 * time.Second):
			return fmt.Errorf("timeout waiting for mpv to exit")
		}
	}

	// fallback: wait with timeout directly on cmd (not ideal if another goroutine will Wait)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("mpv exit error: %w", err)
		}
		return nil
	case <-time.After(3 * time.Second):
		return fmt.Errorf("timeout waiting for mpv to exit")
	}
}
