package main

import (
	"fmt"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/go-toast/toast"
)

const (
	pollIntervalFresh    = 30 * time.Minute // mouse awake  — fresh data
	pollIntervalSleeping = 30 * time.Second  // mouse asleep — retry frequently until it wakes
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

	// refreshCh is signalled when the user clicks "Refresh now".
	// Buffered so a click during a scan is not lost.
	refreshCh := make(chan struct{}, 1)
	go func() {
		for range mRefresh.ClickedCh {
			select {
			case refreshCh <- struct{}{}:
			default:
			}
		}
	}()

	// ── State ────────────────────────────────────────────────────────────────
	var mu sync.Mutex
	prevLevel := 100

	// updateUI applies the latest BatteryInfo to the tray and fires notifications.
	updateUI := func(info BatteryInfo) {
		mu.Lock()
		pl := prevLevel
		if info.Found {
			prevLevel = info.Level
		}
		mu.Unlock()

		mUpdated.SetTitle("Updated: " + time.Now().Format("15:04:05"))

		if !info.Found {
			systray.SetIcon(makeBatteryIcon(0, false))
			systray.SetTooltip("MX Master 3S — not found")
			mStatus.SetTitle("Mouse not found")
			return
		}

		if info.Stale {
			// Data is from Bolt receiver cache — mouse is sleeping.
			// Don't show stale values; wait silently until the mouse wakes.
			systray.SetIcon(makeBatteryIcon(0, false))
			systray.SetTooltip("MX Master 3S — waiting for mouse...")
			mStatus.SetTitle("⏳ Waiting for mouse to wake...")
			return
		}

		// ── Fresh live data from the awake mouse ─────────────────────────────
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
		mStatus.SetTitle(label)
		systray.SetTooltip(fmt.Sprintf("MX Master 3S — %d%%  %s", info.Level, info.Status))
		systray.SetIcon(makeBatteryIcon(info.Level, info.Charging))

		// Toast: fires once when battery crosses below 10% while discharging
		if info.Level < 10 && !info.Charging && pl >= 10 {
			go func(level int) {
				n := toast.Notification{
					AppID:    "MX Master 3S",
					Title:    "⚠️ MX Master 3S — Low Battery",
					Message:  fmt.Sprintf("Battery at %d%%. Please connect the charging cable.", level),
					Duration: toast.Short,
				}
				_ = n.Push()
			}(info.Level)
		}
	}

	// ── Adaptive poll loop ────────────────────────────────────────────────────
	// • Fresh live data  (mouse awake)  → wait 30 min before next poll
	// • Stale/not found  (mouse asleep) → retry every 2 min until mouse wakes
	// • "Refresh now" click             → interrupts the wait immediately
	go func() {
		for {
			mStatus.SetTitle("Scanning...")
			info := ScanMouse()
			updateUI(info)

			var wait time.Duration
			if !info.Found || info.Stale {
				wait = pollIntervalSleeping
			} else {
				wait = pollIntervalFresh
			}

			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
				// scheduled refresh
			case <-refreshCh:
				timer.Stop()
				// user-triggered immediate refresh
			case <-mQuit.ClickedCh:
				timer.Stop()
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {}
