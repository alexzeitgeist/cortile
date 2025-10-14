package input

import (
	"sync"
	"time"

	"github.com/leukipp/cortile/v2/common"
	"github.com/leukipp/cortile/v2/desktop"
	"github.com/leukipp/cortile/v2/store"
	"github.com/leukipp/cortile/v2/ui"

	log "github.com/sirupsen/logrus"
)

var (
	workspace    *desktop.Workspace // Stores previous workspace (for comparison only)
	pointer      *store.XPointer    // Stores previous pointer (for comparison only)
	hover        *time.Timer        // Timer to delay hover events
	dragPollTick *time.Ticker       // Ticker for drag-time polling
	dragPollStop chan struct{}      // Signal to stop drag polling
	dragPollMu   sync.Mutex         // Guards drag polling state
)

func BindMouse(tr *desktop.Tracker) {
	// Start/stop drag-time polling on button transitions
	store.OnPointerUpdate(func(pt store.XPointer, desktop uint, screen uint) {
		if pt.Pressed() {
			startDragPolling(tr)
		} else {
			stopDragPolling()
		}
	})

	// Refresh workspace on EWMH viewport/desktop changes
	store.OnStateUpdate(func(state string, desktop uint, screen uint) {
		if common.IsInList(state, []string{"_NET_CURRENT_DESKTOP", "_NET_DESKTOP_VIEWPORT", "_NET_DESKTOP_GEOMETRY", "_NET_WORKAREA"}) {
			updateWorkspace(tr)
		}
	})
}

func startDragPolling(tr *desktop.Tracker) {
	dragPollMu.Lock()
	defer dragPollMu.Unlock()

	if dragPollTick != nil {
		return // Already polling
	}

	dragPollStop = make(chan struct{})
	dragPollTick = time.NewTicker(100 * time.Millisecond)

	// Capture ticker locally for goroutine
	ticker := dragPollTick

	go func() {
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				store.PointerUpdate(store.X)
				resetTracker(tr)
				pointer = store.Pointer
			case <-dragPollStop:
				return
			}
		}
	}()
}

func stopDragPolling() {
	dragPollMu.Lock()
	defer dragPollMu.Unlock()

	if dragPollTick == nil {
		return // Not polling
	}

	dragPollTick.Stop()
	close(dragPollStop)
	dragPollTick = nil
	dragPollStop = nil
}

func resetTracker(tr *desktop.Tracker) {
	if pointer == nil || pointer.Position != store.Pointer.Position {
		return
	}

	// Reset tracker handler
	if !tr.Handlers.MoveClient.Active() {
		tr.Handlers.Reset()
	}
}

func updateWorkspace(tr *desktop.Tracker) {
	ws := tr.ActiveWorkspace()
	if ws == nil || ws == workspace {
		return
	}
	log.Info("Active workspace updated [", ws.Name, "]")

	// Communicate workplace change
	tr.Channels.Event <- "workplace_change"

	// Update systray icon
	ui.UpdateIcon(ws)

	// Store last workspace
	workspace = ws
}

func updateCorner(tr *desktop.Tracker) {
	// Skip if no corners configured
	if !hasConfiguredCorners() {
		return
	}

	hc := store.HotCorner()
	if hc == nil {
		return
	}

	// Communicate corner change
	tr.Channels.Event <- "corner_change"

	// Execute action
	ExecuteAction(common.Config.Corners[hc.Name], tr, tr.ActiveWorkspace())
}

func updateFocus(tr *desktop.Tracker) {
	// Skip entirely if focus-follows-mouse disabled
	if common.Config.WindowFocusDelay == 0 {
		return
	}

	ws := tr.ActiveWorkspace()
	if ws == nil || pointer == nil || hover != nil {
		return
	}

	// Ignore stationary pointer position
	if pointer.Position == store.Pointer.Position {
		return
	}

	// Ignore untracked clients
	active := tr.ActiveClient()
	hovered := tr.ClientAt(ws, store.Pointer.Position)
	if active == nil || hovered == nil {
		return
	}
	log.Info("Hovered window updated [", hovered.GetLatest().Class, "]")

	// Delay hover event by given duration
	hover = time.AfterFunc(time.Duration(common.Config.WindowFocusDelay)*time.Millisecond, func() {
		hover = nil

		// Hovered client window has changed in the meantime
		if hovered != tr.ClientAt(ws, store.Pointer.Position) {
			return
		}

		// Focus hovered client window
		if hovered != active && ws.TilingEnabled() && !tr.Handlers.Active() {
			store.ActiveWindowSet(store.X, hovered.Window)
		}
	})
}



func hasConfiguredCorners() bool {
	for _, action := range common.Config.Corners {
		if action != "" {
			return true
		}
	}
	return false
}
