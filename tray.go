package main

import (
	"fmt"
	"sync"
	"time"

	"fyne.io/systray"
)

func main() {
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(makeBatteryIcon(0, false))
	systray.SetTooltip("MX Master 3S — starting...")

	// ── Menu layout ──────────────────────────────────────────────────────────
	mHeader := systray.AddMenuItem("MX Master 3S", "")
	mHeader.Disable()

	mStatus := systray.AddMenuItem("Scanning...", "Battery level and status")
	mStatus.Disable()

	mUpdated := systray.AddMenuItem("", "")
	mUpdated.Disable()

	systray.AddSeparator()
	mRefresh := systray.AddMenuItem("Refresh now", "Query the mouse immediately")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Exit MX Master 3S tray")

	// ── Scan logic ───────────────────────────────────────────────────────────
	var mu sync.Mutex
	scanning := false

	doRefresh := func() {
		mu.Lock()
		if scanning {
			mu.Unlock()
			return
		}
		scanning = true
		mu.Unlock()
		defer func() {
			mu.Lock()
			scanning = false
			mu.Unlock()
		}()

		mStatus.SetTitle("Scanning...")

		info := ScanMouse()
		mUpdated.SetTitle("Updated: " + time.Now().Format("15:04:05"))

		if !info.Found {
			systray.SetIcon(makeBatteryIcon(0, false))
			systray.SetTooltip("MX Master 3S — not found")
			mStatus.SetTitle("Mouse not found")
			return
		}

		// Build status line
		var icon string
		switch {
		case info.Charging && info.Level >= 95:
			icon = "🔋"
		case info.Charging:
			icon = "⚡"
		case info.Level < 15:
			icon = "❗"
		default:
			icon = "🔋"
		}

		label := fmt.Sprintf("%s %d%%  —  %s", icon, info.Level, info.Status)
		if info.Stale {
			label += " (cached)"
		}

		mStatus.SetTitle(label)
		systray.SetTooltip(fmt.Sprintf("MX Master 3S — %d%%  %s", info.Level, info.Status))
		systray.SetIcon(makeBatteryIcon(info.Level, info.Charging))
	}

	// Initial scan on startup
	go doRefresh()

	// Ticker: refresh every 5 minutes + handle menu clicks
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for {
			select {
			case <-ticker.C:
				go doRefresh()
			case <-mRefresh.ClickedCh:
				go doRefresh()
			case <-mQuit.ClickedCh:
				ticker.Stop()
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {}
