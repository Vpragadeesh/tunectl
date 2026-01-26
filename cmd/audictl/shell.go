package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	yprov "audictl/providers/youtube"
)

// runShell starts an interactive REPL where the user can search and play tracks.
func runShell() {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("audictl shell — type 'help' for commands")
	var lastResults []map[string]string

	for {
		fmt.Print("audictl> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Fprintln(os.Stderr, "read error:", err)
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		cmd := parts[0]
		args := parts[1:]
		switch cmd {
		case "help":
			printShellHelp()
		case "exit", "quit":
			return
		case "search":
			if len(args) == 0 {
				fmt.Println("usage: search <query>")
				continue
			}
			q := strings.Join(args, " ")
			yt := yprov.New()
			tracks, err := yt.Search(q, 0, 5)
			if err != nil {
				fmt.Fprintln(os.Stderr, "search failed:", err)
				continue
			}
			lastResults = make([]map[string]string, 0, len(tracks))
			for i, t := range tracks {
				fmt.Printf("%d) %s — %s\n", i+1, t.Title, t.Artist)
				lastResults = append(lastResults, map[string]string{"id": t.ID, "title": t.Title, "artist": t.Artist, "link": t.Links["youtube"]})
			}
		case "play":
			if len(args) == 0 {
				fmt.Println("usage: play <n|query|spotify:...>")
				continue
			}
			// if numeric and we have lastResults, play that
			if idx, err := strconv.Atoi(args[0]); err == nil {
				if idx <= 0 || idx > len(lastResults) {
					fmt.Println("index out of range")
					continue
				}
				id := lastResults[idx-1]["id"]
				// send to daemon if available
				if socketExists() {
					req := map[string]interface{}{"cmd": "play", "args": map[string]string{"query": id}}
					if err := sendRPC(req); err != nil {
						fmt.Fprintln(os.Stderr, "rpc error:", err)
					}
				} else {
					// run one-shot
					runPlay(id)
				}
				continue
			}
			// otherwise treat as query/uri
			q := strings.Join(args, " ")
			if socketExists() {
				req := map[string]interface{}{"cmd": "play", "args": map[string]string{"query": q}}
				if err := sendRPC(req); err != nil {
					fmt.Fprintln(os.Stderr, "rpc error:", err)
				}
			} else {
				runPlay(q)
			}
		case "queue.add":
			if len(args) == 0 {
				fmt.Println("usage: queue.add <query>")
				continue
			}
			q := strings.Join(args, " ")
			req := map[string]interface{}{"cmd": "queue.add", "args": map[string]string{"query": q}}
			if err := sendRPC(req); err != nil {
				fmt.Fprintln(os.Stderr, "rpc error:", err)
			}
		case "queue.list":
			req := map[string]interface{}{"cmd": "queue.list"}
			if err := sendRPC(req); err != nil {
				fmt.Fprintln(os.Stderr, "rpc error:", err)
			}
		case "next", "stop", "status":
			req := map[string]interface{}{"cmd": cmd}
			if err := sendRPC(req); err != nil {
				fmt.Fprintln(os.Stderr, "rpc error:", err)
			}
		case "device":
			if len(args) == 0 {
				fmt.Println("usage: device <device-string> (e.g. alsa/hw:0,0)")
				continue
			}
			os.Setenv("AUDICTL_DEVICE", strings.Join(args, " "))
			fmt.Println("AUDICTL_DEVICE set to", os.Getenv("AUDICTL_DEVICE"))
		default:
			fmt.Println("unknown command; try 'help'")
		}
	}
}

func printShellHelp() {
	fmt.Println(`commands:
  search <query>        Search YouTube (top 5)
  play <n|query|uri>    Play result index n or a query/URI
  queue.add <query>     Add query to daemon queue
  queue.list            List queued items
  next                  Skip to next
  stop                  Stop playback (daemon prototype)
  status                Show current and queue
  device <dev>          Set AUDICTL_DEVICE env for future playback
  help                  Show this help
  exit, quit            Exit shell`)
}
