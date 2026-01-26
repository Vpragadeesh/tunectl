package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"audictl/internal/mpv"
	"audictl/internal/provider"
	yprov "audictl/providers/youtube"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type action int

const (
	actionAddToQueue action = iota
	actionNext
	actionPrevious
	actionStop
	actionClearQueue
)

type player struct {
	mu          sync.Mutex
	queue       []provider.Track
	queueIdx    int
	currentCmd  *exec.Cmd
	currentTrk  *provider.Track
	paused      bool
	searching   bool
	stopSpinner chan struct{}
	yt          provider.Provider
	app         *tview.Application
	nowView     *tview.TextView
	queueView   *tview.List
	searchView  *tview.InputField
	resultsView *tview.List
	helpView    *tview.TextView
	searchRes   []provider.Track
	focusables  []tview.Primitive
	focusIdx    int
	actionChan  chan action
}

func main() {
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

	p.resultsView = tview.NewList().ShowSecondaryText(false)
	p.resultsView.SetBorder(true).SetTitle(" Results [Enter=Play, a=Queue] ")
	p.resultsView.SetHighlightFullLine(true)
	p.resultsView.SetSelectedBackgroundColor(tcell.ColorDarkCyan)

	p.nowView = tview.NewTextView()
	p.nowView.SetDynamicColors(true)
	p.nowView.SetBorder(true)
	p.nowView.SetTitle(" Now Playing ")
	p.nowView.SetText("[yellow]No track playing[-]\n\nType to search, press Enter")

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
			"[green]s[-]      Stop           [green]c[-]      Clear queue\n" +
			"[green]Esc[-]    Unfocus search [green]q[-]      Quit",
	)

	// Track focusable items
	p.focusables = []tview.Primitive{p.searchView, p.resultsView, p.queueView}
	p.focusIdx = 0

	// Layout
	searchBox := tview.NewFlex().
		AddItem(nil, 1, 0, false).
		AddItem(p.searchView, 0, 1, true).
		AddItem(nil, 1, 0, false)

	leftPanel := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(searchBox, 3, 0, true).
		AddItem(p.resultsView, 0, 1, false)

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
			p.playTrack(track)
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
		case 'q', 'Q':
			p.cleanup()
			p.app.Stop()
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
			p.playTrack(track)
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
		case 'q', 'Q':
			p.cleanup()
			p.app.Stop()
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

	switch event.Rune() {
	case 'q', 'Q':
		p.cleanup()
		p.app.Stop()
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
		p.paused = false
		p.mu.Unlock()

		dur := ""
		if track.Duration > 0 {
			dur = fmt.Sprintf(" [%d:%02d]", track.Duration/60, track.Duration%60)
		}
		p.updateNowPlaying(fmt.Sprintf("[green]♪ Playing:[-]\n[white]%s[-]\n[gray]%s[-]%s", track.Title, track.Artist, dur))
		p.updateQueueView()

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
	p.mu.Unlock()

	if cmd != nil {
		_ = mpv.KillCmd(cmd)
	}
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

func (p *player) cleanup() {
	p.stop()
	close(p.actionChan)
}
