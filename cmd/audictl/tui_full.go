package main

import (
	"fmt"
	"os"
	"os/exec"
	"sync"

	"audictl/internal/mpv"
	providerpkg "audictl/internal/provider"
	yprov "audictl/providers/youtube"

	tcell "github.com/gdamore/tcell/v2"
	tview "github.com/rivo/tview"
)

// Full-screen TUI: search, results, now-playing, basic controls
func runFullTUI() {
	app := tview.NewApplication()

	search := tview.NewInputField().SetLabel("Search: ")
	results := tview.NewList().ShowSecondaryText(false)
	now := tview.NewTextView()
	now.SetDynamicColors(true)
	now.SetBorder(true)
	now.SetTitle("Now Playing")

	flex := tview.NewFlex().SetDirection(tview.FlexRow)
	flex.AddItem(search, 3, 0, true)
	split := tview.NewFlex()
	split.AddItem(results, 0, 3, false)
	split.AddItem(now, 0, 1, false)
	flex.AddItem(split, 0, 1, false)

	app.SetRoot(flex, true).EnableMouse(true)

	yt := yprov.New()

	var mu sync.Mutex
	var currentCmd *exec.Cmd

	// helper to update now playing
	updateNow := func(text string) {
		app.QueueUpdateDraw(func() { now.SetText(text) })
	}

	// start playback for a chosen track
	startPlayback := func(t providerpkg.Track) {
		updateNow("Resolving...")
		// resolve in background
		go func() {
			stream, err := yt.ResolveStream(t, providerpkg.QualityAny)
			if err != nil {
				updateNow(fmt.Sprintf("Resolve error: %v", err))
				return
			}
			updateNow("Starting mpv...")
			cmd, err := mpv.Start(stream.URL, t.Title, os.Getenv("AUDICTL_DEVICE"), os.Getenv("AUDICTL_RESAMPLE") == "1")
			if err != nil {
				updateNow(fmt.Sprintf("mpv start failed: %v", err))
				return
			}
			// store process
			mu.Lock()
			currentCmd = cmd
			mu.Unlock()

			updateNow(t.Title + " — " + t.Artist)

			// wait in goroutine
			go func() {
				_ = cmd.Wait()
				mu.Lock()
				currentCmd = nil
				mu.Unlock()
				updateNow("Stopped")
			}()
		}()
	}

	// search handler
	search.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			return
		}
		q := search.GetText()
		if q == "" {
			return
		}
		results.Clear()
		updateNow("Searching...")
		go func() {
			res, err := yt.Search(q, providerpkg.SearchKindTrack, 20)
			if err != nil {
				updateNow(fmt.Sprintf("search error: %v", err))
				return
			}
			if len(res) == 0 {
				updateNow("No results")
				return
			}
			app.QueueUpdateDraw(func() {
				for i, tr := range res {
					idx := i
					title := tr.Title + " — " + tr.Artist
					results.AddItem(title, "", 0, func() { startPlayback(res[idx]) })
				}
				updateNow("Ready")
			})
		}()
	})

	// keybindings
	results.SetDoneFunc(func() { app.SetFocus(search) })
	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Rune() {
		case 'q':
			app.Stop()
			return nil
		case 's':
			// stop
			mu.Lock()
			if currentCmd != nil {
				_ = mpv.KillCmd(currentCmd)
			}
			mu.Unlock()
			updateNow("Stopped")
			return nil
		}
		switch ev.Key() {
		case tcell.KeyCtrlC, tcell.KeyEsc:
			app.Stop()
			return nil
		}
		return ev
	})

	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui error:", err)
	}
}
