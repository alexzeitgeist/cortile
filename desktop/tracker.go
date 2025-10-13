package desktop

import (
	"sync"
	"time"

	"github.com/jezek/xgb/xproto"

	"github.com/jezek/xgbutil"
	"github.com/jezek/xgbutil/xevent"
	"github.com/jezek/xgbutil/xprop"

	"github.com/leukipp/cortile/v2/common"
	"github.com/leukipp/cortile/v2/store"

	log "github.com/sirupsen/logrus"
)

const writeDebounce = 750 * time.Millisecond

type writeRequest struct {
	done chan struct{}
}

type Tracker struct {
	Clients     map[xproto.Window]*store.Client // List of tracked clients
	Workspaces  map[store.Location]*Workspace   // List of workspaces per location
	Channels    *Channels                       // Helper for channel communication
	Handlers    *Handlers                       // Helper for event handlers
	lastWrite   time.Time                       // Last time cache was written
	writeDue    bool                            // Pending cache write flag
	writeDueAt  time.Time                       // Scheduled cache write timestamp
	writeQueue  chan writeRequest               // Queue to trigger async writes
	writeExecMu sync.Mutex                      // Serializes write execution
	stateMu     sync.RWMutex                    // Guards Clients and Workspaces maps
	writeMu     sync.Mutex                      // Guards deferred write state
	writeTimer  *time.Timer                     // Timer to flush deferred writes
}
type Channels struct {
	Event  chan string // Channel for events
	Action chan string // Channel for actions
}

type Handlers struct {
	Timer        *time.Timer // Timer to handle delayed structure events
	ResizeClient *Handler    // Stores client for proportion change
	MoveClient   *Handler    // Stores client for tiling after move
	SwapClient   *Handler    // Stores clients for window swap
	SwapScreen   *Handler    // Stores client for screen swap
}

func (h *Handlers) Active() bool {
	return h.ResizeClient.Active() || h.MoveClient.Active() || h.SwapClient.Active() || h.SwapScreen.Active()
}

func (h *Handlers) Reset() {
	h.ResizeClient.Reset()
	h.MoveClient.Reset()
	h.SwapClient.Reset()
	h.SwapScreen.Reset()
}

type Handler struct {
	Dragging bool        // Indicates pointer dragging event
	Source   interface{} // Stores moved/resized client
	Target   interface{} // Stores client/workspace
}

func (h *Handler) Active() bool {
	return h.Source != nil
}

func (h *Handler) Reset() {
	*h = Handler{}
}

func CreateTracker() *Tracker {
	tr := Tracker{
		Clients:    make(map[xproto.Window]*store.Client),
		Workspaces: CreateWorkspaces(),
		Channels: &Channels{
			Event:  make(chan string),
			Action: make(chan string),
		},
		Handlers: &Handlers{
			ResizeClient: &Handler{},
			MoveClient:   &Handler{},
			SwapClient:   &Handler{},
			SwapScreen:   &Handler{},
		},
		writeQueue: make(chan writeRequest, 1),
	}

	// Start background writer
	go tr.backgroundWriter()

	// Attach to root events
	store.OnStateUpdate(tr.onStateUpdate)
	store.OnPointerUpdate(tr.onPointerUpdate)

	return &tr
}

func (tr *Tracker) Update() {
	start := time.Now()
	ws := tr.ActiveWorkspace()
	if ws.TilingDisabled() {
		return
	}
	tracked := tr.snapshotClients()
	log.Debug("Update trackable clients [", len(tracked), "/", len(store.Windows.Stacked), "]")

	added := 0
	updated := 0
	skipped := 0
	ignored := 0

	// Map trackable windows
	trackable := make(map[xproto.Window]bool)
	for _, w := range store.Windows.Stacked {
		trackable[w.Id] = tr.isTrackable(w.Id)
	}

	// Remove untrackable windows and update tracked ones
	removed := 0
	for w, existing := range tracked {
		if !trackable[w] {
			tr.untrackWindow(w)
			removed++
		} else {
			// Only update clients on the current desktop to avoid unnecessary X11 calls
			if existing != nil && existing.Latest != nil && existing.Latest.Location.Desktop == store.Workplace.CurrentDesktop {
				existing.Update()
				updated++
			} else {
				skipped++
			}
		}
	}

	// Add trackable windows
	for _, w := range store.Windows.Stacked {
		if trackable[w.Id] {
			if !tr.isTracked(w.Id) {
				tr.trackWindow(w.Id)
				added++
			}
		} else {
			ignored++
		}
	}

	log.WithFields(log.Fields{
		"currentDesk": store.Workplace.CurrentDesktop,
		"tracked":     len(tracked),
		"windows":     len(store.Windows.Stacked),
		"added":       added,
		"updated":     updated,
		"skipped":     skipped,
		"ignored":     ignored,
		"removed":     removed,
		"elapsed":     time.Since(start),
	}).Debug("tracker.update.stats")
}

func (tr *Tracker) Reset() {
	tracked := tr.snapshotClients()
	log.Debug("Reset trackable clients [", len(tracked), "/", len(store.Windows.Stacked), "]")

	// Reset client list
	for w := range tracked {
		tr.untrackWindow(w)
	}

	// Reset workspaces
	tr.stateMu.Lock()
	tr.Workspaces = CreateWorkspaces()
	tr.stateMu.Unlock()

	// Communicate workplace change
	tr.Channels.Event <- "workplace_change"
}

func (tr *Tracker) backgroundWriter() {
	for req := range tr.writeQueue {
		tr.doWrite()
		if req.done != nil {
			close(req.done)
		}
	}
}

func (tr *Tracker) Write() {
	tr.enqueueWrite(false)
}

func (tr *Tracker) Flush() {
	tr.writeMu.Lock()
	if tr.writeTimer != nil {
		tr.writeTimer.Stop()
		tr.writeTimer = nil
	}
	tr.writeMu.Unlock()

	tr.enqueueWrite(true)
}

func (tr *Tracker) doWrite() {
	tr.writeExecMu.Lock()
	defer tr.writeExecMu.Unlock()

	start := time.Now()

	tr.writeMu.Lock()
	tr.writeDue = false
	if tr.writeTimer != nil {
		tr.writeTimer.Stop()
		tr.writeTimer = nil
	}
	tr.writeMu.Unlock()

	// Count dirty items before writing
	clients := tr.snapshotClientList()
	workspaces := tr.snapshotWorkspaceList()

	clientsDirty := 0
	for _, c := range clients {
		if c.IsDirty() {
			clientsDirty++
		}
	}
	workspacesDirty := 0
	for _, ws := range workspaces {
		if ws.IsDirty() {
			workspacesDirty++
		}
	}

	log.WithFields(log.Fields{
		"clients":         len(clients),
		"clientsDirty":    clientsDirty,
		"workspaces":      len(workspaces),
		"workspacesDirty": workspacesDirty,
		"desk":            store.Workplace.CurrentDesktop,
	}).Debug("tracker.write.start")

	// Write client cache (only dirty clients)
	for _, c := range clients {
		c.Write()
	}

	// Write workspace cache (only dirty workspaces)
	for _, ws := range workspaces {
		ws.Write()
	}

	elapsed := time.Since(start)
	log.WithFields(log.Fields{
		"clients":           len(clients),
		"clientsWritten":    clientsDirty,
		"workspaces":        len(workspaces),
		"workspacesWritten": workspacesDirty,
		"elapsed":           elapsed,
	}).Debug("tracker.write.complete")

	tr.writeMu.Lock()
	tr.lastWrite = time.Now()
	tr.writeMu.Unlock()

	// Communicate windows change
	tr.Channels.Event <- "windows_change"
}

func (tr *Tracker) enqueueWrite(wait bool) {
	req := writeRequest{}
	if wait {
		req.done = make(chan struct{})
		tr.writeQueue <- req
		<-req.done
		return
	}
	select {
	case tr.writeQueue <- req:
		log.Debug("tracker.write.enqueued")
	default:
		log.Trace("tracker.write.already-queued")
	}
}

func (tr *Tracker) Tile(ws *Workspace) {
	if ws.TilingDisabled() {
		return
	}

	// Tile workspace
	ws.Tile()

	// Communicate clients change
	tr.Channels.Event <- "clients_change"

	// Communicate workspaces change
	tr.Channels.Event <- "workspaces_change"
}

func (tr *Tracker) Restore(ws *Workspace, flag uint8) {

	// Restore workspace
	ws.Restore(flag)

	// Communicate clients change
	tr.Channels.Event <- "clients_change"

	// Communicate workspaces change
	tr.Channels.Event <- "workspaces_change"
}

func (tr *Tracker) ActiveWorkspace() *Workspace {
	if store.Workplace == nil {
		return nil
	}
	return tr.WorkspaceAt(store.Workplace.CurrentDesktop, store.Workplace.CurrentScreen)
}

func (tr *Tracker) ClientWorkspace(c *store.Client) *Workspace {
	if c == nil {
		return nil
	}
	return tr.WorkspaceAt(c.Latest.Location.Desktop, c.Latest.Location.Screen)
}

func (tr *Tracker) WorkspaceAt(desktop uint, screen uint) *Workspace {
	location := store.Location{Desktop: desktop, Screen: screen}

	// Validate workspace
	tr.stateMu.RLock()
	ws := tr.Workspaces[location]
	tr.stateMu.RUnlock()
	if ws == nil {
		log.Warn("Invalid workspace [workspace-", location.Desktop, "-", location.Screen, "]")
	}

	return ws
}

func (tr *Tracker) ClientAt(ws *Workspace, p common.Point) *store.Client {
	if ws == nil {
		return nil
	}

	// Check if point hovers visible client
	for _, c := range ws.VisibleClients() {
		if c == nil {
			continue
		}
		if common.IsInsideRect(p, c.Latest.Dimensions.Geometry) {
			return c
		}
	}

	return nil
}

func (tr *Tracker) clientByWindow(id xproto.Window) (*store.Client, bool) {
	tr.stateMu.RLock()
	defer tr.stateMu.RUnlock()
	c, ok := tr.Clients[id]
	return c, ok
}

func (tr *Tracker) snapshotClients() map[xproto.Window]*store.Client {
	tr.stateMu.RLock()
	defer tr.stateMu.RUnlock()
	snapshot := make(map[xproto.Window]*store.Client, len(tr.Clients))
	for id, client := range tr.Clients {
		snapshot[id] = client
	}
	return snapshot
}

func (tr *Tracker) snapshotClientList() []*store.Client {
	tr.stateMu.RLock()
	defer tr.stateMu.RUnlock()
	clients := make([]*store.Client, 0, len(tr.Clients))
	for _, c := range tr.Clients {
		clients = append(clients, c)
	}
	return clients
}

func (tr *Tracker) snapshotWorkspaceList() []*Workspace {
	tr.stateMu.RLock()
	defer tr.stateMu.RUnlock()
	workspaces := make([]*Workspace, 0, len(tr.Workspaces))
	for _, ws := range tr.Workspaces {
		workspaces = append(workspaces, ws)
	}
	return workspaces
}

func (tr *Tracker) clientCount() int {
	tr.stateMu.RLock()
	defer tr.stateMu.RUnlock()
	return len(tr.Clients)
}

func (tr *Tracker) workspaceCount() int {
	tr.stateMu.RLock()
	defer tr.stateMu.RUnlock()
	return len(tr.Workspaces)
}

func (tr *Tracker) ActiveClient() *store.Client {
	c, exists := tr.clientByWindow(store.Windows.Active.Id)
	if !exists {
		return nil
	}
	return c
}

func (tr *Tracker) unlockClients() {
	ws := tr.ActiveWorkspace()
	if ws == nil {
		return
	}

	// Unlock clients
	mg := ws.ActiveLayout().GetManager()
	for _, c := range mg.Clients(store.Stacked) {
		if c == nil {
			continue
		}
		c.UnLock()
	}
}

func (tr *Tracker) trackWindow(w xproto.Window) bool {
	if tr.isTracked(w) {
		return false
	}

	// Client and workspace
	c := store.CreateClient(w)
	ws := tr.ClientWorkspace(c)
	if ws == nil {
		return false
	}

	// Add new client
	tr.stateMu.Lock()
	if _, exists := tr.Clients[c.Window.Id]; exists {
		tr.stateMu.Unlock()
		return false
	}
	tr.Clients[c.Window.Id] = c
	tr.stateMu.Unlock()
	ws.AddClient(c)

	// Attach handlers
	tr.attachHandlers(c)
	tr.Tile(ws)

	return true
}

func (tr *Tracker) untrackWindow(w xproto.Window) bool {
	if !tr.isTracked(w) {
		return false
	}

	tr.stateMu.Lock()
	c, exists := tr.Clients[w]
	if !exists {
		tr.stateMu.Unlock()
		return false
	}
	delete(tr.Clients, w)
	tr.stateMu.Unlock()

	ws := tr.ClientWorkspace(c)
	if ws == nil {
		return false
	}

	// Detach events
	xevent.Detach(store.X, w)

	// Restore client
	c.Restore(store.Latest)

	// Remove client
	ws.RemoveClient(c)

	// Tile workspace
	tr.Tile(ws)

	return true
}

func (tr *Tracker) handleMaximizedClient(c *store.Client) {
	if !tr.isTracked(c.Window.Id) {
		return
	}

	// Client maximized
	if store.IsMaximized(store.GetInfo(c.Window.Id)) {
		ws := tr.ClientWorkspace(c)
		if ws.TilingDisabled() {
			return
		}
		log.Debug("Client maximized handler fired [", c.Latest.Class, "]")

		// Update client states
		c.Update()

		// Unmaximize window
		c.UnMaximize()

		// Activate maximized layout
		if !c.IsNew() && ws.ActiveLayout().GetName() != "maximized" {
			tr.Channels.Action <- "layout_maximized"
			store.ActiveWindowSet(store.X, c.Window)
		}
	}
}

func (tr *Tracker) handleMinimizedClient(c *store.Client) {
	if !tr.isTracked(c.Window.Id) {
		return
	}

	ws := tr.ClientWorkspace(c)
	if ws == nil || ws.TilingDisabled() {
		return
	}

	hidden := store.IsMinimized(store.GetInfo(c.Window.Id))

	if hidden {
		log.Debug("Client minimized, untracking [", c.Latest.Class, "]")
		tr.untrackWindow(c.Window.Id)
		return
	}
}

func (tr *Tracker) handleResizeClient(c *store.Client) {
	ws := tr.ClientWorkspace(c)
	if ws.TilingDisabled() || !tr.isTracked(c.Window.Id) || store.IsMaximized(store.GetInfo(c.Window.Id)) {
		return
	}

	// Previous dimensions
	pGeom := c.Latest.Dimensions.Geometry
	px, py, pw, ph := pGeom.Pieces()

	// Current dimensions
	cGeom, err := c.Window.Instance.DecorGeometry()
	if err != nil {
		return
	}
	cx, cy, cw, ch := cGeom.Pieces()

	// Check size changes
	resized := cw != pw || ch != ph
	moved := (cx != px || cy != py) && (cw == pw && ch == ph)

	if resized && !moved && !tr.Handlers.MoveClient.Active() {
		pt := store.PointerUpdate(store.X)

		// Set client resize event
		if !c.IsNew() && !tr.Handlers.ResizeClient.Active() {
			tr.Handlers.ResizeClient = &Handler{Dragging: pt.Dragging(500), Source: c}
		}
		log.Debug("Client resize handler fired [", c.Latest.Class, "]")

		if tr.Handlers.ResizeClient.Dragging {

			// Set client resize lock
			if tr.Handlers.ResizeClient.Active() {
				tr.Handlers.ResizeClient.Source.(*store.Client).Lock()
				log.Debug("Client resize handler active [", c.Latest.Class, "]")
			}

			// Update proportions
			dir := &store.Directions{
				Top:    cy != py,
				Right:  cx == px && cw != pw,
				Bottom: cy == py && ch != ph,
				Left:   cx != px,
			}
			ws.ActiveLayout().UpdateProportions(c, dir)
			ws.MarkDirty()
			tr.ScheduleWrite()
		}

		// Tile workspace
		tr.Tile(ws)
	}
}

func (tr *Tracker) handleMoveClient(c *store.Client) {
	ws := tr.ClientWorkspace(c)
	if !tr.isTracked(c.Window.Id) || store.IsMaximized(store.GetInfo(c.Window.Id)) {
		return
	}

	// Previous dimensions
	pGeom := c.Latest.Dimensions.Geometry
	px, py, pw, ph := pGeom.Pieces()

	// Current dimensions
	cGeom, err := c.Window.Instance.DecorGeometry()
	if err != nil {
		return
	}
	cx, cy, cw, ch := cGeom.Pieces()

	// Check position changes
	moved := cx != px || cy != py
	resized := cw != pw || ch != ph

	if moved && !resized && !tr.Handlers.ResizeClient.Active() {
		pt := store.PointerUpdate(store.X)

		// Set client move event
		if !c.IsNew() && !tr.Handlers.MoveClient.Active() {
			tr.Handlers.MoveClient = &Handler{Dragging: pt.Dragging(500), Source: c}
		}
		log.Debug("Client move handler fired [", c.Latest.Class, "]")

		// Obtain targets based on dragging indicator
		targetPoint := *common.CreatePoint(cx, cy)
		if tr.Handlers.MoveClient.Dragging {
			targetPoint = pt.Position
		}
		targetDesktop := store.Workplace.CurrentDesktop
		targetScreen := store.ScreenGet(targetPoint)

		// Check if target point hovers another client
		tr.Handlers.SwapClient.Reset()
		if co := tr.ClientAt(ws, targetPoint); co != nil && co != c {
			tr.Handlers.SwapClient = &Handler{Source: c, Target: co}
			log.Debug("Client swap handler active [", c.Latest.Class, "-", co.Latest.Class, "]")
		}

		// Check if target point moves to another screen
		tr.Handlers.SwapScreen.Reset()
		if c.Latest.Location.Screen != targetScreen {
			tr.Handlers.SwapScreen = &Handler{Source: c, Target: tr.WorkspaceAt(targetDesktop, targetScreen)}
			log.Debug("Screen swap handler active [", c.Latest.Class, "]")
		}
	}
}

func (tr *Tracker) handleSwapClient(h *Handler) {
	c, target := h.Source.(*store.Client), h.Target.(*store.Client)
	ws := tr.ClientWorkspace(c)
	if !tr.isTracked(c.Window.Id) {
		return
	}
	log.Debug("Client swap handler fired [", c.Latest.Class, "-", target.Latest.Class, "]")

	// Swap clients on same desktop and screen
	mg := ws.ActiveLayout().GetManager()
	mg.SwapClient(c, target)

	// Mark workspace and clients dirty
	ws.MarkDirty()
	c.MarkDirty()
	target.MarkDirty()
	tr.ScheduleWrite()

	// Reset client swapping handler
	h.Reset()

	// Tile workspace
	tr.Tile(ws)
}

func (tr *Tracker) handleWorkspaceChange(h *Handler) {
	c, target := h.Source.(*store.Client), h.Target.(*Workspace)
	if !tr.isTracked(c.Window.Id) {
		return
	}
	log.Debug("Client workspace handler fired [", c.Latest.Class, "]")

	// Remove client from current workspace
	ws := tr.ClientWorkspace(c)
	mg := ws.ActiveLayout().GetManager()
	master := mg.IsMaster(c)
	ws.RemoveClient(c)

	// Tile current workspace
	if ws.TilingEnabled() {
		tr.Tile(ws)
	}

	// Update client desktop and screen
	if !tr.isTrackable(c.Window.Id) {
		return
	}
	c.Update()

	// Add client to new workspace
	ws = tr.ClientWorkspace(c)
	if tr.Handlers.SwapScreen.Active() && target.TilingEnabled() {
		ws = target
	}
	mg = ws.ActiveLayout().GetManager()
	ws.AddClient(c)
	if master {
		mg.MakeMaster(c)
		ws.MarkDirty()
		c.MarkDirty()
		tr.ScheduleWrite()
	}

	// Tile new workspace
	if ws.TilingEnabled() {
		tr.Tile(ws)
	} else {
		c.Restore(store.Latest)
	}

	// Reset screen swapping handler
	h.Reset()
}

func (tr *Tracker) onStateUpdate(state string, desktop uint, screen uint) {
	start := time.Now()
	workplaceChanged := store.Workplace.DesktopCount*store.Workplace.ScreenCount != uint(tr.workspaceCount())
	workspaceChanged := common.IsInList(state, []string{"_NET_CURRENT_DESKTOP"})

	viewportChanged := common.IsInList(state, []string{"_NET_NUMBER_OF_DESKTOPS", "_NET_DESKTOP_LAYOUT", "_NET_DESKTOP_GEOMETRY", "_NET_DESKTOP_VIEWPORT", "_NET_WORKAREA"})
	clientListChanged := common.IsInList(state, []string{"_NET_CLIENT_LIST_STACKING"})
	focusChanged := common.IsInList(state, []string{"_NET_ACTIVE_WINDOW"})
	clientsChanged := clientListChanged || focusChanged

	if workplaceChanged {

		// Reset clients and workspaces
		tr.Reset()
	}

	if workspaceChanged {

		// Update sticky windows
		for _, c := range tr.snapshotClientList() {
			if store.IsSticky(c.Latest) && c.Latest.Location.Desktop != store.Workplace.CurrentDesktop {
				c.MoveToDesktop(^uint32(0))
			}
		}
	}

	if workspaceChanged || viewportChanged || clientsChanged {

		// Deactivate handlers
		tr.Handlers.Reset()

		// Unlock clients
		tr.unlockClients()

		// Update trackable clients
		tr.Update()
	}

	// Persist cache only when topology really changed
	if workplaceChanged || clientListChanged {
		tr.ScheduleWrite()
	}

	tr.maybeWrite()

	elapsed := time.Since(start)
	if elapsed > 5*time.Millisecond {
		log.WithFields(log.Fields{
			"event":   state,
			"elapsed": elapsed,
		}).Debug("tracker.onStateUpdate")
	}
}

func (tr *Tracker) onPointerUpdate(pointer store.XPointer, desktop uint, screen uint) {
	buttonReleased := !pointer.Pressed()

	// Reset timer
	if tr.Handlers.Timer != nil {
		tr.Handlers.Timer.Stop()
	}

	// Wait on button release
	var t time.Duration = 0
	if buttonReleased {
		t = 50
	}

	// Wait for structure events
	tr.Handlers.Timer = time.AfterFunc(t*time.Millisecond, func() {

		// Window moved to another screen
		if tr.Handlers.SwapScreen.Active() {
			tr.handleWorkspaceChange(tr.Handlers.SwapScreen)
		}

		// Window moved over another window
		if tr.Handlers.SwapClient.Active() {
			tr.handleSwapClient(tr.Handlers.SwapClient)
		}

		// Window moved or resized
		if tr.Handlers.MoveClient.Active() || tr.Handlers.ResizeClient.Active() {
			tr.Handlers.MoveClient.Reset()
			tr.Handlers.ResizeClient.Reset()

			// Unlock clients
			tr.unlockClients()

			// Tile workspace
			if buttonReleased {
				tr.Tile(tr.ActiveWorkspace())
			}
		}
	})
}

func (tr *Tracker) attachHandlers(c *store.Client) {
	c.Window.Instance.Listen(xproto.EventMaskStructureNotify | xproto.EventMaskPropertyChange | xproto.EventMaskFocusChange)

	// Attach structure events
	xevent.ConfigureNotifyFun(func(X *xgbutil.XUtil, ev xevent.ConfigureNotifyEvent) {
		log.Trace("Client structure event [", c.Latest.Class, "]")

		// Handle structure events
		tr.handleResizeClient(c)
		tr.handleMoveClient(c)
		if !tr.Handlers.MoveClient.Active() {
			c.Update()
		}
	}).Connect(store.X, c.Window.Id)

	// Attach property events
	xevent.PropertyNotifyFun(func(X *xgbutil.XUtil, ev xevent.PropertyNotifyEvent) {
		aname, _ := xprop.AtomName(store.X, ev.Atom)
		log.Trace("Client property event ", aname, " [", c.Latest.Class, "]")

		// Handle property events
		if aname == "_NET_WM_STATE" {
			tr.handleMaximizedClient(c)
			tr.handleMinimizedClient(c)
		} else if aname == "_NET_WM_DESKTOP" {
			tr.handleWorkspaceChange(&Handler{Source: c, Target: tr.ActiveWorkspace()})
		}
	}).Connect(store.X, c.Window.Id)
}

func (tr *Tracker) isTracked(w xproto.Window) bool {
	tr.stateMu.RLock()
	_, ok := tr.Clients[w]
	tr.stateMu.RUnlock()
	return ok
}

func (tr *Tracker) isTrackable(w xproto.Window) bool {
	info := store.GetInfo(w)
	return tr.isTrackableInfo(info)
}

func (tr *Tracker) isTrackableInfo(info *store.Info) bool {
	if info == nil {
		return false
	}
	// Allow hidden windows on other desktops to remain trackable
	if common.IsInList("_NET_WM_STATE_HIDDEN", info.States) && info.Location.Desktop != store.Workplace.CurrentDesktop {
		return !store.IsIgnored(info)
	}
	return !store.IsSpecial(info) && !store.IsIgnored(info)
}

func (tr *Tracker) ScheduleWrite() {
	deadline := time.Now().Add(writeDebounce)
	tr.writeMu.Lock()
	if !tr.writeDue || deadline.Before(tr.writeDueAt) {
		tr.writeDueAt = deadline
	}
	tr.writeDue = true

	delay := time.Until(tr.writeDueAt)
	if delay < 0 {
		delay = 0
	}
	if tr.writeTimer != nil {
		tr.writeTimer.Stop()
	}
	tr.writeTimer = time.AfterFunc(delay, tr.flushScheduledWrite)
	scheduledAt := tr.writeDueAt
	tr.writeMu.Unlock()

	log.WithFields(log.Fields{
		"deadline": scheduledAt,
	}).Trace("tracker.write.scheduled")
}

func (tr *Tracker) maybeWrite() {
	tr.writeMu.Lock()
	if !tr.writeDue {
		tr.writeMu.Unlock()
		return
	}
	remaining := time.Until(tr.writeDueAt)
	tr.writeMu.Unlock()
	if remaining > 0 {
		log.WithField("remaining", remaining).Trace("tracker.write.debounce")
		return
	}
	tr.flushScheduledWrite()
}

func (tr *Tracker) flushScheduledWrite() {
	tr.writeMu.Lock()
	if !tr.writeDue {
		if tr.writeTimer != nil {
			tr.writeTimer.Stop()
			tr.writeTimer = nil
		}
		tr.writeMu.Unlock()
		return
	}
	remaining := time.Until(tr.writeDueAt)
	if remaining > 0 {
		if tr.writeTimer != nil {
			tr.writeTimer.Stop()
		}
		tr.writeTimer = time.AfterFunc(remaining, tr.flushScheduledWrite)
		tr.writeMu.Unlock()
		return
	}
	tr.writeTimer = nil
	tr.writeMu.Unlock()

	tr.Write()
}
