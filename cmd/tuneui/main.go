package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"audictl/internal/mpv"
	"audictl/internal/provider"
	sprov "audictl/providers/spotify"
	yprov "audictl/providers/youtube"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// urlList is a simple flag.Value to collect multiple --url / -u flags
type urlList []string

func (u *urlList) String() string {
	return strings.Join(*u, ",")
}

func (u *urlList) Set(value string) error {
	*u = append(*u, value)
	return nil
}

type action int

const (
	actionAddToQueue action = iota
	actionNext
	actionPrevious
	actionStop
	actionClearQueue
	actionPlay
	actionPause
	actionFastForward
	actionRewind
	actionForceQuit
)

type player struct {
	mu            sync.Mutex
	queue         []provider.Track
	queueIdx      int
	currentCmd    *exec.Cmd
	currentTrk    *provider.Track
	playbackStart time.Time
	paused        bool
	searching     bool
	stopSpinner   chan struct{}
	stopProgress  chan struct{}
	yt            provider.Provider
	app           *tview.Application
	nowView       *tview.TextView
	progressView  *tview.TextView
	queueView     *tview.List
	searchView    *tview.InputField
	linkView      *tview.InputField
	resultsView   *tview.List
	helpView      *tview.TextView
	searchRes     []provider.Track
	focusables    []tview.Primitive
	focusIdx      int
	actionChan    chan action
}

func main() {
	// Parse startup flags
	var urls urlList
	flag.Var(&urls, "url", "URL to open on startup (may be repeated)")
	flag.Var(&urls, "u", "shorthand for --url")
	flag.Parse()

	app := tview.NewApplication()
	p := &player{
		queue:      []provider.Track{},
		yt:         yprov.New(),
		app:        app,
		actionChan: make(chan action, 10),
	}

	// Create UI components
	p.searchView = tview.NewInputField()
	p.searchView.SetLabel(" Search: ")
	p.searchView.SetFieldWidth(0)
	p.searchView.SetFieldBackgroundColor(tcell.ColorDarkSlateGray)

	p.linkView = tview.NewInputField()
	p.linkView.SetLabel(" Paste link: ")
	p.linkView.SetFieldWidth(0)
	p.linkView.SetFieldBackgroundColor(tcell.ColorDarkSlateGray)
	p.linkView.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			link := strings.TrimSpace(p.linkView.GetText())
			if link != "" {
				// Process in goroutine so we don't block the UI
				go p.handleLink(link)
				p.linkView.SetText("")
			}
		case tcell.KeyEsc, tcell.KeyTab, tcell.KeyBacktab:
			// handled by global
		}
	})

	p.resultsView = tview.NewList().ShowSecondaryText(false)
	p.resultsView.SetBorder(true).SetTitle(" Results [Enter=Play, a=Queue] ")
	p.resultsView.SetHighlightFullLine(true)
	p.resultsView.SetSelectedBackgroundColor(tcell.ColorDarkCyan)

	p.nowView = tview.NewTextView()
	p.nowView.SetDynamicColors(true)
	p.nowView.SetBorder(true)
	p.nowView.SetTitle(" Now Playing ")
	p.nowView.SetText("[yellow]No track playing[-]\n\nType to search, press Enter")

	p.progressView = tview.NewTextView()
	p.progressView.SetDynamicColors(true)
	p.progressView.SetBorder(true)
	p.progressView.SetTitle(" Progress ")
	p.progressView.SetText("")

	p.queueView = tview.NewList().ShowSecondaryText(false)
	p.queueView.SetBorder(true).SetTitle(" Queue [Enter=Play] ")
	p.queueView.SetHighlightFullLine(true)
	p.queueView.SetSelectedBackgroundColor(tcell.ColorDarkCyan)

	p.helpView = tview.NewTextView()
	p.helpView.SetDynamicColors(true)
	p.helpView.SetBorder(true)
	p.helpView.SetTitle(" Controls ")
	p.helpView.SetText(
		"[green]Tab[-]    Next panel    [green]S-Tab[-]  Prev panel\n" +
			"[green]Enter[-]  Play selected  [green]a[-]      Add to queue\n" +
			"[green]n[-]      Next track     [green]p[-]      Prev track\n" +
			"[green]Space[-]  Play/Pause     [green]s[-]      Stop\n" +
			"[green]→ ←[-]    Fwd/Rewind     [green]c[-]      Clear queue\n" +
			"[green]Esc[-]    Unfocus        [green]q[-]      Force Quit\n" +
			"\n" +
			"[yellow]YouTube:[-] yt.be/xxx or youtube.com/...\n" +
			"[yellow]Spotify:[-] open.spotify.com/track/xxx [gray](→ searches YouTube)[-]",
	)

	// Track focusable items
	p.focusables = []tview.Primitive{p.searchView, p.linkView, p.resultsView, p.queueView}
	p.focusIdx = 0

	// Layout
	searchBox := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tview.NewFlex().
			AddItem(nil, 1, 0, false).
			AddItem(p.searchView, 0, 1, true).
			AddItem(nil, 1, 0, false), 3, 0, true).
		AddItem(tview.NewFlex().
			AddItem(nil, 1, 0, false).
			AddItem(p.linkView, 0, 1, false).
			AddItem(nil, 1, 0, false), 3, 0, false)

	leftPanel := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(searchBox, 3, 0, true).
		AddItem(p.resultsView, 0, 1, false).
		AddItem(p.progressView, 3, 0, false)

	rightPanel := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(p.nowView, 0, 2, false).
		AddItem(p.queueView, 0, 3, false).
		AddItem(p.helpView, 7, 0, false)

	mainFlex := tview.NewFlex().
		AddItem(leftPanel, 0, 2, true).
		AddItem(rightPanel, 0, 1, false)

	app.SetRoot(mainFlex, true).EnableMouse(true)

	// Setup handlers
	p.setupHandlers()

	// Set initial focus
	app.SetFocus(p.searchView)

	// Start action processor
	go p.processActions()

	// If startup URLs were provided, process them shortly after initialization.
	// Behavior: multiple occurrences allowed. Single-track single-URL will play immediately.
	if len(urls) > 0 {
		go func() {
			// Small delay to ensure UI has initialised enough for updates
			time.Sleep(150 * time.Millisecond)
			for i, link := range urls {
				link = strings.TrimSpace(link)
				if link == "" {
					continue
				}

				// Debug print so CLI users see what's happening on startup
				fmt.Fprintf(os.Stderr, "startup: processing url [%d]: %s\n", i+1, link)

				// YouTube
				if strings.Contains(link, "youtube.com") || strings.Contains(link, "youtu.be") {
					y := yprov.New()
					tracks, err := y.FetchTracksFromURL(link, 0)
					if err != nil {
						fmt.Fprintf(os.Stderr, "startup: youtube extraction error: %v\n", err)
						p.updateNowPlaying(fmt.Sprintf("[red]Link error:[-] %v", err))
						continue
					}
					fmt.Fprintf(os.Stderr, "startup: youtube returned %d tracks\n", len(tracks))
					if len(tracks) == 0 {
						p.updateNowPlaying("[yellow]No tracks found in link[-]")
						continue
					}
					// If single URL and single track, auto-play
					if len(tracks) == 1 && len(urls) == 1 {
						go p.playTrack(tracks[0])
						continue
					}
					p.mu.Lock()
					p.queue = append(p.queue, tracks...)
					p.mu.Unlock()
					p.updateQueueView()
					p.updateNowPlaying(fmt.Sprintf("[green]+ Added playlist:[-] %d tracks", len(tracks)))
					continue
				}

				// Spotify
				if strings.Contains(link, "spotify.com") {
					fmt.Fprintf(os.Stderr, "startup: spotify url -> %s\n", link)
					sp := sprov.New()
					tracks, err := sp.FetchTracksFromURL(link)
					if err != nil {
						fmt.Fprintf(os.Stderr, "startup: spotify extraction error: %v\n", err)
						p.updateNowPlaying(fmt.Sprintf("[red]Spotify error:[-] %v", err))
						continue
					}
					fmt.Fprintf(os.Stderr, "startup: spotify returned %d tracks\n", len(tracks))
					if len(tracks) == 0 {
						p.updateNowPlaying("[yellow]No tracks found in Spotify link[-]")
						continue
					}
					if len(tracks) == 1 && len(urls) == 1 {
						go p.playTrack(tracks[0])
						continue
					}
					p.mu.Lock()
					p.queue = append(p.queue, tracks...)
					p.mu.Unlock()
					p.updateQueueView()
					if len(tracks) == 1 {
						p.updateNowPlaying(fmt.Sprintf("[green]+ Added:[-] %s", tracks[0].Title))
					} else {
						p.updateNowPlaying(fmt.Sprintf("[green]+ Added playlist:[-] %d items", len(tracks)))
					}
					continue
				}

				// Unsupported
				p.updateNowPlaying("[yellow]Unsupported link type[-]")
				_ = i
			}
		}()
	}

	// Handle system signals
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		p.cleanup()
		app.Stop()
	}()

	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		os.Exit(1)
	}
}

func (p *player) setupHandlers() {
	// Search input - Enter to search, Esc to leave
	p.searchView.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			query := p.searchView.GetText()
			if query != "" {
				p.performSearch(query)
			}
		case tcell.KeyEsc, tcell.KeyTab, tcell.KeyBacktab:
			// handled by global
		}
	})

	// Results list - Enter plays
	p.resultsView.SetSelectedFunc(func(idx int, primary string, secondary string, shortcut rune) {
		p.mu.Lock()
		if idx >= 0 && idx < len(p.searchRes) {
			track := p.searchRes[idx]
			p.mu.Unlock()
			// Spawn in goroutine to avoid blocking tview event loop
			go p.playTrack(track)
		} else {
			p.mu.Unlock()
		}
	})

	// Intercept keys on results list
	p.resultsView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case 'a', 'A':
			p.actionChan <- actionAddToQueue
			return nil
		case 'n', 'N':
			p.actionChan <- actionNext
			return nil
		case 'p', 'P':
			p.actionChan <- actionPrevious
			return nil
		case 's', 'S':
			p.actionChan <- actionStop
			return nil
		case 'c', 'C':
			p.actionChan <- actionClearQueue
			return nil
		case ' ':
			p.actionChan <- actionPause
			return nil
		case 'q', 'Q':
			p.actionChan <- actionForceQuit
			return nil
		}
		switch event.Key() {
		case tcell.KeyRight:
			p.actionChan <- actionFastForward
			return nil
		case tcell.KeyLeft:
			p.actionChan <- actionRewind
			return nil
		}
		return p.handleGlobalKey(event)
	})

	// Queue list
	p.queueView.SetSelectedFunc(func(idx int, primary string, secondary string, shortcut rune) {
		p.mu.Lock()
		if idx >= 0 && idx < len(p.queue) {
			track := p.queue[idx]
			p.queueIdx = idx
			p.mu.Unlock()
			// Spawn in goroutine to avoid blocking tview event loop
			go p.playTrack(track)
		} else {
			p.mu.Unlock()
		}
	})

	// Intercept keys on queue list
	p.queueView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case 'n', 'N':
			p.actionChan <- actionNext
			return nil
		case 'p', 'P':
			p.actionChan <- actionPrevious
			return nil
		case 's', 'S':
			p.actionChan <- actionStop
			return nil
		case 'c', 'C':
			p.actionChan <- actionClearQueue
			return nil
		case ' ':
			p.actionChan <- actionPause
			return nil
		case 'q', 'Q':
			p.actionChan <- actionForceQuit
			return nil
		}
		switch event.Key() {
		case tcell.KeyRight:
			p.actionChan <- actionFastForward
			return nil
		case tcell.KeyLeft:
			p.actionChan <- actionRewind
			return nil
		}
		return p.handleGlobalKey(event)
	})

	// Global input capture
	p.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		focused := p.app.GetFocus()

		// If in search box, only intercept Tab/Esc/Ctrl+C
		if focused == p.searchView {
			switch event.Key() {
			case tcell.KeyTab:
				p.nextFocus()
				return nil
			case tcell.KeyBacktab:
				p.prevFocus()
				return nil
			case tcell.KeyEsc:
				p.nextFocus()
				return nil
			case tcell.KeyCtrlC:
				p.cleanup()
				p.app.Stop()
				return nil
			}
			return event
		}

		return p.handleGlobalKey(event)
	})
}

func (p *player) handleGlobalKey(event *tcell.EventKey) *tcell.EventKey {
	switch event.Key() {
	case tcell.KeyCtrlC:
		p.cleanup()
		p.app.Stop()
		return nil
	case tcell.KeyCtrlQ:
		p.actionChan <- actionForceQuit
		return nil
	case tcell.KeyTab:
		p.nextFocus()
		return nil
	case tcell.KeyBacktab:
		p.prevFocus()
		return nil
	case tcell.KeyEsc:
		p.app.SetFocus(p.resultsView)
		return nil
	}

	return event
}

func (p *player) processActions() {
	for action := range p.actionChan {
		switch action {
		case actionAddToQueue:
			p.addToQueue()
		case actionNext:
			p.next()
		case actionPrevious:
			p.previous()
		case actionStop:
			p.stop()
			p.updateNowPlaying("[yellow]Stopped[-]")
		case actionClearQueue:
			p.clearQueue()
		case actionPlay:
			mpv.Play()
		case actionPause:
			mpv.Pause()
		case actionFastForward:
			mpv.Seek(10) // Skip forward 10 seconds
		case actionRewind:
			mpv.Seek(-10) // Rewind 10 seconds
		case actionForceQuit:
			p.forceQuit()
		}
	}
}

func (p *player) nextFocus() {
	p.focusIdx = (p.focusIdx + 1) % len(p.focusables)
	p.app.SetFocus(p.focusables[p.focusIdx])
}

func (p *player) prevFocus() {
	p.focusIdx--
	if p.focusIdx < 0 {
		p.focusIdx = len(p.focusables) - 1
	}
	p.app.SetFocus(p.focusables[p.focusIdx])
}

func (p *player) addToQueue() {
	focused := p.app.GetFocus()
	if focused != p.resultsView {
		p.updateNowPlaying("[yellow]Select a result first (Tab to results, then 'a')[-]")
		return
	}

	idx := p.resultsView.GetCurrentItem()
	p.mu.Lock()
	if idx < 0 || idx >= len(p.searchRes) {
		p.mu.Unlock()
		p.updateNowPlaying("[yellow]No result selected[-]")
		return
	}
	track := p.searchRes[idx]
	p.queue = append(p.queue, track)
	title := track.Title
	p.mu.Unlock()

	p.updateQueueView()
	p.updateNowPlaying(fmt.Sprintf("[green]+ Added:[-] %s", title))
}

func (p *player) performSearch(query string) {
	p.mu.Lock()
	if p.stopSpinner != nil {
		close(p.stopSpinner)
	}
	p.stopSpinner = make(chan struct{})
	p.searching = true
	stopCh := p.stopSpinner
	p.mu.Unlock()

	p.resultsView.Clear()

	// Start spinner animation
	go func() {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		i := 0
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				p.app.QueueUpdateDraw(func() {
					p.nowView.SetText(fmt.Sprintf("[yellow]%s Searching for '%s'...[-]", frames[i], query))
				})
				i = (i + 1) % len(frames)
			}
		}
	}()

	go func() {
		results, err := p.yt.Search(query, provider.SearchKindTrack, 10)

		p.mu.Lock()
		if p.stopSpinner == stopCh {
			close(p.stopSpinner)
			p.stopSpinner = nil
		}
		p.searching = false
		p.mu.Unlock()

		if err != nil {
			p.updateNowPlaying(fmt.Sprintf("[red]Search error:[-] %v", err))
			return
		}
		if len(results) == 0 {
			p.updateNowPlaying("[yellow]No results found[-]")
			return
		}

		p.mu.Lock()
		p.searchRes = results
		p.mu.Unlock()

		p.app.QueueUpdateDraw(func() {
			p.resultsView.Clear()
			for i, track := range results {
				dur := ""
				if track.Duration > 0 {
					dur = fmt.Sprintf(" [%d:%02d]", track.Duration/60, track.Duration%60)
				}
				title := fmt.Sprintf("%d. %s - %s%s", i+1, track.Artist, track.Title, dur)
				p.resultsView.AddItem(title, "", 0, nil)
			}
			p.focusIdx = 1
			p.app.SetFocus(p.resultsView)
			p.nowView.SetText(fmt.Sprintf("[green]✓ Found %d results[-]\n\nUse [yellow]↑/↓[-] to navigate\n[yellow]Enter[-] to play, [yellow]a[-] to queue", len(results)))
		})
	}()
}

// handleLink processes pasted links (YouTube/Spotify). It accepts single videos/tracks as well
// as playlists. For playlists, all entries are added to the queue; single tracks are played
// (YouTube) or added to the queue (Spotify metadata, DRM).
func (p *player) handleLink(link string) {
	link = strings.TrimSpace(link)
	if link == "" {
		return
	}

	// YouTube links (video or playlist)
	if strings.Contains(link, "youtube.com") || strings.Contains(link, "youtu.be") {
		y := yprov.New()
		tracks, err := y.FetchTracksFromURL(link, 0)
		if err != nil {
			p.updateNowPlaying(fmt.Sprintf("[red]Link error:[-] %v", err))
			return
		}
		if len(tracks) == 0 {
			p.updateNowPlaying("[yellow]No tracks found in link[-]")
			return
		}
		if len(tracks) == 1 {
			go p.playTrack(tracks[0])
			return
		}
		p.mu.Lock()
		p.queue = append(p.queue, tracks...)
		p.mu.Unlock()
		p.updateQueueView()
		p.updateNowPlaying(fmt.Sprintf("[green]+ Added playlist:[-] %d tracks", len(tracks)))
		return
	}

	// Spotify links (track or playlist)
	if strings.Contains(link, "spotify.com") {
		sp := sprov.New()
		tracks, err := sp.FetchTracksFromURL(link)
		if err != nil {
			p.updateNowPlaying(fmt.Sprintf("[red]Spotify error:[-] %v", err))
			return
		}
		if len(tracks) == 0 {
			p.updateNowPlaying("[yellow]No tracks found in Spotify link[-]")
			return
		}

		// Add all tracks to queue (don't auto-play Spotify due to auth requirements)
		p.mu.Lock()
		p.queue = append(p.queue, tracks...)
		p.mu.Unlock()
		p.updateQueueView()

		if len(tracks) == 1 {
			p.updateNowPlaying(fmt.Sprintf("[yellow]⚠ Spotify added (requires premium + auth):[-]\n%s", tracks[0].Title))
		} else {
			p.updateNowPlaying(fmt.Sprintf("[yellow]⚠ Spotify added (requires premium + auth):[-]\n%d items", len(tracks)))
		}
		return
	}

	p.updateNowPlaying("[yellow]Unsupported link type[-]")
}

func (p *player) playTrack(track provider.Track) {
	p.stop()

	p.mu.Lock()
	if p.stopSpinner != nil {
		close(p.stopSpinner)
	}
	p.stopSpinner = make(chan struct{})
	stopCh := p.stopSpinner
	p.mu.Unlock()

	go func() {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		i := 0
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				p.app.QueueUpdateDraw(func() {
					p.nowView.SetText(fmt.Sprintf("[yellow]%s Loading:[-]\n[white]%s[-]\n[gray]%s[-]", frames[i], track.Title, track.Artist))
				})
				i = (i + 1) % len(frames)
			}
		}
	}()

	go func() {
		stream, err := p.yt.ResolveStream(track, provider.QualityAny)

		p.mu.Lock()
		if p.stopSpinner == stopCh {
			close(p.stopSpinner)
			p.stopSpinner = nil
		}
		p.mu.Unlock()

		if err != nil {
			p.updateNowPlaying(fmt.Sprintf("[red]Resolve error:[-] %v", err))
			return
		}

		device := os.Getenv("AUDICTL_DEVICE")
		resample := os.Getenv("AUDICTL_RESAMPLE") == "1"
		cmd, err := mpv.Start(stream.URL, track.Title, device, resample)
		if err != nil {
			p.updateNowPlaying(fmt.Sprintf("[red]mpv error:[-] %v", err))
			return
		}

		p.mu.Lock()
		p.currentCmd = cmd
		p.currentTrk = &track
		p.playbackStart = time.Now()
		p.paused = false
		if p.stopProgress != nil {
			close(p.stopProgress)
		}
		p.stopProgress = make(chan struct{})
		stopProgressCh := p.stopProgress
		p.mu.Unlock()

		dur := ""
		if track.Duration > 0 {
			dur = fmt.Sprintf(" [%d:%02d]", track.Duration/60, track.Duration%60)
		}
		p.updateNowPlaying(fmt.Sprintf("[green]♪ Playing:[-]\n[white]%s[-]\n[gray]%s[-]%s", track.Title, track.Artist, dur))
		p.updateQueueView()

		// Start progress bar updater
		go p.updateProgress(track, stopProgressCh)

		go func() {
			_ = cmd.Wait()
			p.mu.Lock()
			wasCurrent := p.currentCmd == cmd
			if wasCurrent {
				p.currentCmd = nil
				p.currentTrk = nil
			}
			p.mu.Unlock()

			if wasCurrent {
				p.updateNowPlaying("[gray]Track finished[-]")
				time.Sleep(500 * time.Millisecond)
				p.next()
			}
		}()
	}()
}

func (p *player) stop() {
	p.mu.Lock()
	cmd := p.currentCmd
	p.currentCmd = nil
	p.currentTrk = nil
	if p.stopProgress != nil {
		close(p.stopProgress)
		p.stopProgress = nil
	}
	p.mu.Unlock()

	if cmd != nil {
		_ = mpv.KillCmd(cmd)
	}

	// Clear progress bar
	p.app.QueueUpdateDraw(func() {
		p.progressView.SetText("")
	})
}

func (p *player) next() {
	p.mu.Lock()
	if len(p.queue) == 0 {
		p.mu.Unlock()
		p.updateNowPlaying("[yellow]Queue is empty - add songs with 'a'[-]")
		return
	}

	p.queueIdx++
	if p.queueIdx >= len(p.queue) {
		p.queueIdx = 0
	}
	track := p.queue[p.queueIdx]
	p.mu.Unlock()

	p.playTrack(track)
}

func (p *player) previous() {
	p.mu.Lock()
	if len(p.queue) == 0 {
		p.mu.Unlock()
		p.updateNowPlaying("[yellow]Queue is empty - add songs with 'a'[-]")
		return
	}

	p.queueIdx--
	if p.queueIdx < 0 {
		p.queueIdx = len(p.queue) - 1
	}
	track := p.queue[p.queueIdx]
	p.mu.Unlock()

	p.playTrack(track)
}

func (p *player) clearQueue() {
	p.mu.Lock()
	p.queue = []provider.Track{}
	p.queueIdx = 0
	p.mu.Unlock()
	p.updateQueueView()
	p.updateNowPlaying("[green]Queue cleared[-]")
}

func (p *player) updateQueueView() {
	p.mu.Lock()
	queueCopy := make([]provider.Track, len(p.queue))
	copy(queueCopy, p.queue)
	currentTrk := p.currentTrk
	p.mu.Unlock()

	p.app.QueueUpdateDraw(func() {
		p.queueView.Clear()
		for i, track := range queueCopy {
			prefix := "  "
			if currentTrk != nil && track.ID == currentTrk.ID {
				prefix = "► "
			}
			dur := ""
			if track.Duration > 0 {
				dur = fmt.Sprintf(" [%d:%02d]", track.Duration/60, track.Duration%60)
			}
			title := fmt.Sprintf("%s%d. %s%s", prefix, i+1, track.Title, dur)
			p.queueView.AddItem(title, "", 0, nil)
		}
	})
}

func (p *player) updateNowPlaying(text string) {
	p.app.QueueUpdateDraw(func() {
		p.nowView.SetText(text)
	})
}

func (p *player) updateProgress(track provider.Track, stopCh chan struct{}) {
	if stopCh == nil || track.Duration <= 0 {
		p.app.QueueUpdateDraw(func() {
			p.progressView.SetText("")
		})
		return
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			p.mu.Lock()
			if p.currentCmd == nil || p.currentTrk == nil {
				p.mu.Unlock()
				return
			}
			elapsed := time.Since(p.playbackStart).Seconds()
			total := float64(track.Duration)
			p.mu.Unlock()

			// Clamp elapsed to 0-total
			if elapsed < 0 {
				elapsed = 0
			}
			if elapsed > total {
				elapsed = total
			}
			// Calculate progress bar - use full width of box
			_, _, width, _ := p.progressView.GetRect()
			barWidth := width - 4 // Account for borders and padding
			if barWidth < 10 {
				barWidth = 10
			}

			progress := int((elapsed / total) * float64(barWidth))
			if progress > barWidth {
				progress = barWidth
			}

			// Build progress bar with colored sections
			filledBar := ""
			for i := 0; i < progress; i++ {
				filledBar += "█" // Solid blocks for filled portion
			}

			remainingBar := ""
			for i := progress; i < barWidth; i++ {
				remainingBar += "·" // Dots for unfilled portion
			}

			elapsedMin := int(elapsed) / 60
			elapsedSec := int(elapsed) % 60
			totalMin := track.Duration / 60
			totalSec := track.Duration % 60
			percentage := int((elapsed / total) * 100)

			progressText := fmt.Sprintf("[aqua:black:b]%s[-:black] %s %d%% %d:%02d / %d:%02d (%d%%)",
				filledBar, remainingBar, percentage, elapsedMin, elapsedSec, totalMin, totalSec, percentage)

			p.app.QueueUpdateDraw(func() {
				p.progressView.SetText(progressText)
			})
		}
	}
}

func (p *player) forceQuit() {
	// Force quit everything within 1 second
	go func() {
		p.mu.Lock()
		if p.currentCmd != nil && p.currentCmd.Process != nil {
			// Kill the mpv process immediately
			_ = p.currentCmd.Process.Kill()
		}
		p.mu.Unlock()

		// Stop the app
		p.app.Stop()
	}()

	// Exit forcefully after 1 second if still running
	time.AfterFunc(1*time.Second, func() {
		os.Exit(0)
	})
}

func (p *player) cleanup() {
	p.stop()
	close(p.actionChan)
}
