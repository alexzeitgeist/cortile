package store

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"encoding/json"
	"path/filepath"

	"github.com/jezek/xgb/xproto"

	"github.com/jezek/xgbutil/ewmh"
	"github.com/jezek/xgbutil/icccm"
	"github.com/jezek/xgbutil/motif"
	"github.com/jezek/xgbutil/xprop"
	"github.com/jezek/xgbutil/xrect"
	"github.com/jezek/xgbutil/xwindow"

	"github.com/leukipp/cortile/v2/common"

	log "github.com/sirupsen/logrus"
)

type Client struct {
	Window   *XWindow  // X window object
	Created  time.Time // Internal client creation time
	Locked   bool      // Internal client move/resize lock
	Original *Info     `json:"-"` // Original client window information
	Cached   *Info     `json:"-"` // Cached client window information
	Latest   *Info     // Latest client window information (for JSON)
	latestMu sync.RWMutex
	mu       sync.Mutex
	dirty    bool // Internal flag for cache write optimization
}

func (c *Client) GetLatest() *Info {
	c.latestMu.RLock()
	defer c.latestMu.RUnlock()
	return c.Latest
}

func (c *Client) setLatest(info *Info) {
	c.latestMu.Lock()
	c.Latest = info
	c.latestMu.Unlock()
}

type Info struct {
	Class      string     // Client window application name
	Name       string     // Client window title name
	Types      []string   // Client window types
	States     []string   // Client window states
	Location   Location   // Client window location
	Dimensions Dimensions // Client window dimensions
}

type Dimensions struct {
	Geometry   common.Geometry   // Client window geometry
	Hints      Hints             // Client window dimension hints
	Extents    ewmh.FrameExtents // Client window geometry extents
	AdjPos     bool              // Position adjustments on move/resize
	AdjSize    bool              // Size adjustments on move/resize
	AdjRestore bool              // Disable adjustments on restore
}

type Hints struct {
	Normal icccm.NormalHints // Client window geometry hints
	Motif  motif.Hints       // Client window decoration hints
}

const (
	Original uint8 = 1 // Flag to restore original info
	Cached   uint8 = 2 // Flag to restore cached info
	Latest   uint8 = 3 // Flag to restore latest info
)

func CreateClient(w xproto.Window) *Client {
	original := GetInfo(w)
	cached := GetInfo(w)
	latest := GetInfo(w)
	c := &Client{
		Window:   CreateXWindow(w),
		Created:  time.Now(),
		Locked:   false,
		Original: original,
		Cached:   cached,
		dirty:    true,
	}
	c.setLatest(latest)

	cachedData := c.Read()

	c.Cached.States = cachedData.GetLatest().States
	c.Cached.Dimensions.Geometry = cachedData.GetLatest().Dimensions.Geometry
	c.Cached.Location.Screen = ScreenGet(cachedData.GetLatest().Dimensions.Geometry.Center())

	c.Restore(Cached)

	latestInfo := c.GetLatest()
	latestInfo.States = c.Cached.States
	latestInfo.Dimensions.Geometry = c.Cached.Dimensions.Geometry
	latestInfo.Location.Screen = c.Cached.Location.Screen

	return c
}

func (c *Client) Lock() {
	c.Locked = true
}

func (c *Client) UnLock() {
	c.Locked = false
}

func (c *Client) MarkDirty() {
	c.mu.Lock()
	c.dirty = true
	c.mu.Unlock()
}

func (c *Client) IsDirty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirty
}

// filterPersistentStates returns only states that matter for cache persistence.
// Transient states like focus or attention don't affect window restoration.
func filterPersistentStates(states []string) []string {
	persistent := make([]string, 0, len(states))
	for _, state := range states {
		// Only keep states that affect geometry or need restoration
		switch state {
		case "_NET_WM_STATE_MAXIMIZED_VERT",
			"_NET_WM_STATE_MAXIMIZED_HORZ",
			"_NET_WM_STATE_FULLSCREEN",
			"_NET_WM_STATE_HIDDEN",
			"_NET_WM_STATE_STICKY",
			"_NET_WM_STATE_SHADED",
			"_NET_WM_STATE_SKIP_TASKBAR",
			"_NET_WM_STATE_SKIP_PAGER",
			"_NET_WM_STATE_ABOVE",
			"_NET_WM_STATE_BELOW":
			persistent = append(persistent, state)
		// Skip transient states like:
		// - _NET_WM_STATE_FOCUSED (changes with every focus)
		// - _NET_WM_STATE_DEMANDS_ATTENTION (temporary notification state)
		}
	}
	return persistent
}

func (c *Client) Limit(w, h int) bool {
	if !Compatible("icccm.SizeHintPMinSize") {
		return false
	}

	ext := c.GetLatest().Dimensions.Extents
	dw, dh := ext.Left+ext.Right, ext.Top+ext.Bottom

	nhints := c.Cached.Dimensions.Hints.Normal
	nhints.Flags |= icccm.SizeHintPMinSize
	nhints.MinWidth = uint(w - dw)
	nhints.MinHeight = uint(h - dh)
	icccm.WmNormalHintsSet(X, c.Window.Id, &nhints)

	return true
}

func (c *Client) UnLimit() bool {
	if !Compatible("icccm.SizeHintPMinSize") {
		return false
	}

	// Restore window size limits
	icccm.WmNormalHintsSet(X, c.Window.Id, &c.Cached.Dimensions.Hints.Normal)

	return true
}

func (c *Client) Decorate() bool {
	if _, exists := common.Config.Keys["decoration"]; !exists {
		return false
	}
	latest := c.GetLatest()
	if motif.Decor(&latest.Dimensions.Hints.Motif) || !motif.Decor(&c.Original.Dimensions.Hints.Motif) {
		return false
	}

	mhints := c.Cached.Dimensions.Hints.Motif
	mhints.Flags |= motif.HintDecorations
	mhints.Decoration = motif.DecorationAll
	motif.WmHintsSet(X, c.Window.Id, &mhints)

	return true
}

func (c *Client) UnDecorate() bool {
	if _, exists := common.Config.Keys["decoration"]; !exists {
		return false
	}
	latest := c.GetLatest()
	if !motif.Decor(&latest.Dimensions.Hints.Motif) && motif.Decor(&c.Original.Dimensions.Hints.Motif) {
		return false
	}

	mhints := c.Cached.Dimensions.Hints.Motif
	mhints.Flags |= motif.HintDecorations
	mhints.Decoration = motif.DecorationNone
	motif.WmHintsSet(X, c.Window.Id, &mhints)

	return true
}

func (c *Client) Fullscreen() bool {
	if IsFullscreen(c.GetLatest()) {
		return false
	}

	ewmh.WmStateReq(X, c.Window.Id, ewmh.StateAdd, "_NET_WM_STATE_FULLSCREEN")

	return true
}

func (c *Client) UnFullscreen() bool {
	if !IsFullscreen(c.GetLatest()) {
		return false
	}

	ewmh.WmStateReq(X, c.Window.Id, ewmh.StateRemove, "_NET_WM_STATE_FULLSCREEN")

	return true
}

func (c *Client) UnMaximize() bool {
	if !IsMaximized(c.GetLatest()) {
		return false
	}

	ewmh.WmStateReq(X, c.Window.Id, ewmh.StateRemove, "_NET_WM_STATE_MAXIMIZED_VERT")
	ewmh.WmStateReq(X, c.Window.Id, ewmh.StateRemove, "_NET_WM_STATE_MAXIMIZED_HORZ")

	return true
}

func (c *Client) MoveToDesktop(desktop uint32) bool {
	if desktop == ^uint32(0) {
		ewmh.WmStateReq(X, c.Window.Id, ewmh.StateAdd, "_NET_WM_STATE_STICKY")
	}

	// Set client desktop
	ewmh.WmDesktopSet(X, c.Window.Id, uint(desktop))
	ewmh.ClientEvent(X, c.Window.Id, "_NET_WM_DESKTOP", int(desktop), int(2))

	return true
}

func (c *Client) MoveToScreen(screen uint32) bool {
	geom := Workplace.Displays.Screens[screen].Geometry

	// Calculate move to position
	_, _, w, h := c.OuterGeometry()
	x, y := common.MaxInt(geom.Center().X-w/2, geom.X+100), common.MaxInt(geom.Center().Y-h/2, geom.Y+100)

	// Move window and simulate tracker pointer press
	ewmh.MoveWindow(X, c.Window.Id, x, y)
	Pointer.Press()

	return true
}

func (c *Client) MoveWindow(x, y, w, h int) {
	if c.Locked {
		log.Info("Reject window move/resize [", c.GetLatest().Class, "]")

		c.UnLock()
		return
	}

	c.UnMaximize()
	c.UnFullscreen()

	latest := c.GetLatest()
	ext := latest.Dimensions.Extents
	dx, dy, dw, dh := 0, 0, 0, 0

	if latest.Dimensions.AdjPos {
		dx, dy = ext.Left, ext.Top
	}
	if latest.Dimensions.AdjSize {
		dw, dh = ext.Left+ext.Right, ext.Top+ext.Bottom
	}

	if w > 0 && h > 0 {
		ewmh.MoveresizeWindow(X, c.Window.Id, x+dx, y+dy, w-dw, h-dh)
	} else {
		ewmh.MoveWindow(X, c.Window.Id, x+dx, y+dy)
	}

	c.Update()
}

func (c *Client) OuterGeometry() (x, y, w, h int) {

	oGeom, err := c.Window.Instance.DecorGeometry()
	if err != nil {
		return
	}

	iGeom, err := xwindow.RawGeometry(X, xproto.Drawable(c.Window.Id))
	if err != nil {
		return
	}

	if reflect.DeepEqual(oGeom, iGeom) {
		iGeom.XSet(0)
		iGeom.YSet(0)
	}

	ext := c.GetLatest().Dimensions.Extents
	dx, dy, dw, dh := ext.Left, ext.Top, ext.Left+ext.Right, ext.Top+ext.Bottom

	x, y, w, h = oGeom.X()+iGeom.X()-dx, oGeom.Y()+iGeom.Y()-dy, iGeom.Width()+dw, iGeom.Height()+dh

	return
}

func (c *Client) Restore(flag uint8) {
	if flag == Latest {
		c.Update()
	}

	// Restore window states
	if flag == Cached {
		if IsSticky(c.Cached) {
			c.MoveToDesktop(^uint32(0))
		}
	}

	// Restore window sizes
	c.UnLimit()
	c.UnMaximize()
	c.UnFullscreen()

	// Restore window decorations
	if flag == Original {
		if common.Config.WindowDecoration {
			c.Decorate()
		} else {
			c.UnDecorate()
		}
		c.Update()
	}

	latest := c.GetLatest()
	if latest.Dimensions.AdjRestore {
		c.latestMu.Lock()
		c.Latest.Dimensions.AdjPos = false
		c.Latest.Dimensions.AdjSize = false
		c.latestMu.Unlock()
	}

	geom := latest.Dimensions.Geometry
	switch flag {
	case Original:
		geom = c.Original.Dimensions.Geometry
	case Cached:
		geom = c.Cached.Dimensions.Geometry
	}
	c.MoveWindow(geom.X, geom.Y, geom.Width, geom.Height)
}

func (c *Client) Update() {
	start := time.Now()
	info := GetInfo(c.Window.Id)
	elapsed := time.Since(start)
	if len(info.Class) == 0 {
		return
	}
	log.WithFields(log.Fields{
		"class":   info.Class,
		"elapsed": elapsed,
	}).Debug("client.update")

	oldInfo := c.GetLatest()
	if oldInfo != nil {
		geomChanged := !reflect.DeepEqual(info.Dimensions.Geometry, oldInfo.Dimensions.Geometry)

		oldPersistentStates := filterPersistentStates(oldInfo.States)
		newPersistentStates := filterPersistentStates(info.States)
		statesChanged := !reflect.DeepEqual(newPersistentStates, oldPersistentStates)

		locationChanged := info.Location.Desktop != oldInfo.Location.Desktop ||
			info.Location.Screen != oldInfo.Location.Screen

		if geomChanged || statesChanged || locationChanged {
			c.mu.Lock()
			c.dirty = true
			c.mu.Unlock()
			log.WithFields(log.Fields{
				"class": info.Class,
				"geom":  geomChanged,
				"state": statesChanged,
				"loc":   locationChanged,
			}).Trace("client.marked.dirty")
		}
	}

	c.setLatest(info)
}

func (c *Client) Write() {
	if common.CacheDisabled() {
		return
	}

	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		latest := c.GetLatest()
		log.Trace("Skip clean client cache write [", latest.Class, "]")
		return
	}
	c.mu.Unlock()

	start := time.Now()

	// Create serialization snapshot under lock protection
	type SerializableClient struct {
		Window  *XWindow
		Created time.Time
		Locked  bool
		Latest  *Info
	}

	c.mu.Lock()
	c.latestMu.RLock()
	snapshot := &SerializableClient{
		Window:  c.Window,
		Created: c.Created,
		Locked:  c.Locked,
		Latest:  c.Latest,
	}
	c.latestMu.RUnlock()
	c.mu.Unlock()

	latest := snapshot.Latest
	cache := c.Cache()
	log.WithFields(log.Fields{
		"client": latest.Class,
		"desk":   latest.Location.Desktop,
		"path":   cache.Name,
	}).Debug("client.cache.write.start")

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		log.Warn("Error parsing client cache [", latest.Class, "]")
		return
	}

	path := filepath.Join(cache.Folder, cache.Name)
	tmp, err := os.CreateTemp(cache.Folder, cache.Name+".tmp-*")
	if err != nil {
		log.Warn("Error creating client cache temp file [", latest.Class, "]")
		return
	}
	tmpName := tmp.Name()
	closed := false
	cleanup := func() {
		if !closed {
			tmp.Close()
		}
		os.Remove(tmpName)
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		log.Warn("Error writing client cache temp file [", latest.Class, "]")
		return
	}
	if err = tmp.Sync(); err != nil {
		log.Warn("Error syncing client cache temp file [", latest.Class, "]")
		return
	}
	if err = tmp.Close(); err != nil {
		log.Warn("Error closing client cache temp file [", latest.Class, "]")
		return
	}
	closed = true
	if err = os.Chmod(tmpName, 0644); err != nil {
		log.Warn("Error chmod client cache temp file [", latest.Class, "]")
		return
	}
	if err = os.Rename(tmpName, path); err != nil {
		log.Warn("Error replacing client cache [", latest.Class, "]")
		return
	}
	cleanup = nil

	c.mu.Lock()
	c.dirty = false
	c.mu.Unlock()

	elapsed := time.Since(start)
	log.WithFields(log.Fields{
		"client":  latest.Class,
		"path":    cache.Name,
		"elapsed": elapsed,
	}).Debug("client.cache.write.complete")
}

func (c *Client) Read() *Client {
	if common.CacheDisabled() {
		return c
	}

	cache := c.Cache()
	latest := c.GetLatest()

	path := filepath.Join(cache.Folder, cache.Name)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		log.Info("No client cache found [", latest.Class, "]")
		return c
	}
	if err != nil {
		log.Warn("Error opening client cache [", latest.Class, "]")
		return c
	}
	if len(data) == 0 {
		log.Warn("Empty client cache [", latest.Class, "]")
		return c
	}

	cached := &Client{}
	err = json.Unmarshal([]byte(data), &cached)
	if err != nil {
		log.Warn("Error reading client cache [", latest.Class, "]")
		return c
	}

	log.Debug("Read client cache data ", cache.Name, " [", latest.Class, "]")

	return cached
}

func (c *Client) Cache() common.Cache[*Client] {
	latest := c.GetLatest()
	subfolder := latest.Class
	filename := fmt.Sprintf("%s-%d", subfolder, latest.Location.Desktop)

	folder := filepath.Join(common.Args.Cache, "workplaces", Workplace.Displays.Name, "clients", subfolder)
	if _, err := os.Stat(folder); os.IsNotExist(err) {
		os.MkdirAll(folder, 0755)
	}

	cache := common.Cache[*Client]{
		Folder: folder,
		Name:   common.HashString(filename, 20) + ".json",
		Data:   c,
	}

	return cache
}

func (c *Client) IsNew() bool {
	created := time.UnixMilli(c.Window.Created)
	return time.Since(created) < 1000*time.Millisecond
}

func IsSpecial(info *Info) bool {

	// Check internal windows
	if info.Class == common.Build.Name {
		log.Info("Ignore internal window [", info.Class, "]")
		return true
	}

	// Check window types
	types := []string{
		"_NET_WM_WINDOW_TYPE_DOCK",
		"_NET_WM_WINDOW_TYPE_DESKTOP",
		"_NET_WM_WINDOW_TYPE_TOOLBAR",
		"_NET_WM_WINDOW_TYPE_UTILITY",
		"_NET_WM_WINDOW_TYPE_TOOLTIP",
		"_NET_WM_WINDOW_TYPE_SPLASH",
		"_NET_WM_WINDOW_TYPE_DIALOG",
		"_NET_WM_WINDOW_TYPE_COMBO",
		"_NET_WM_WINDOW_TYPE_NOTIFICATION",
		"_NET_WM_WINDOW_TYPE_DROPDOWN_MENU",
		"_NET_WM_WINDOW_TYPE_POPUP_MENU",
		"_NET_WM_WINDOW_TYPE_MENU",
		"_NET_WM_WINDOW_TYPE_DND",
	}
	for _, typ := range info.Types {
		if common.IsInList(typ, types) {
			log.Info("Ignore window with type ", typ, " [", info.Class, "]")
			return true
		}
	}

	// Check window states
	states := []string{
		"_NET_WM_STATE_HIDDEN",
		"_NET_WM_STATE_MODAL",
		"_NET_WM_STATE_ABOVE",
		"_NET_WM_STATE_BELOW",
		"_NET_WM_STATE_SKIP_PAGER",
		"_NET_WM_STATE_SKIP_TASKBAR",
	}
	for _, state := range info.States {
		// Allow hidden windows on other desktops to remain trackable
		if state == "_NET_WM_STATE_HIDDEN" && info.Location.Desktop != Workplace.CurrentDesktop {
			continue
		}
		if common.IsInList(state, states) {
			log.Info("Ignore window with state ", state, " [", info.Class, "]")
			return true
		}
	}

	return false
}

func IsIgnored(info *Info) bool {

	// Check invalid windows
	if len(info.Class) == 0 {
		log.Info("Ignore invalid window")
		return true
	}

	// Check ignored windows
	for _, s := range common.Config.WindowIgnore {
		conf_class := s[0]
		conf_name := s[1]

		reg_class := regexp.MustCompile(strings.ToLower(conf_class))
		reg_name := regexp.MustCompile(strings.ToLower(conf_name))

		// Ignore all windows with this class
		class_match := reg_class.MatchString(strings.ToLower(info.Class))

		// But allow the window with a special name
		name_match := conf_name != "" && reg_name.MatchString(strings.ToLower(info.Name))

		if class_match && !name_match {
			log.Info("Ignore window with ", strings.TrimSpace(strings.Join(s, " ")), " from config [", info.Class, "]")
			return true
		}
	}

	return false
}

func IsFullscreen(info *Info) bool {
	return common.IsInList("_NET_WM_STATE_FULLSCREEN", info.States)
}

func IsMaximized(info *Info) bool {
	return common.IsInList("_NET_WM_STATE_MAXIMIZED_VERT", info.States) || common.IsInList("_NET_WM_STATE_MAXIMIZED_HORZ", info.States)
}

func IsMinimized(info *Info) bool {
	return common.IsInList("_NET_WM_STATE_HIDDEN", info.States)
}

func IsSticky(info *Info) bool {
	return common.IsInList("_NET_WM_STATE_STICKY", info.States)
}

func GetInfo(w xproto.Window) *Info {
	var err error

	var class string
	var name string
	var types []string
	var states []string
	var location Location
	var dimensions Dimensions

	// Window class (internal class name of the window)
	cls, err := icccm.WmClassGet(X, w)
	if err != nil {
		log.Trace("Error on request: ", err)
	} else if cls != nil {
		class = cls.Class
	}

	// Window name (title on top of the window)
	name, err = icccm.WmNameGet(X, w)
	if err != nil {
		name = class
	}

	// Window geometry (dimensions of the window)
	geom, err := CreateXWindow(w).Instance.DecorGeometry()
	if err != nil {
		geom = &xrect.XRect{}
	}

	// Window desktop and screen (window workspace location)
	desktop, err := ewmh.WmDesktopGet(X, w)
	sticky := desktop > Workplace.DesktopCount
	if err != nil || sticky {
		desktop = CurrentDesktopGet(X)
	}
	location = Location{
		Desktop: desktop,
		Screen:  ScreenGet(common.CreateGeometry(geom).Center()),
	}

	// Window types (types of the window)
	types, err = ewmh.WmWindowTypeGet(X, w)
	if err != nil {
		types = []string{}
	}

	// Window states (states of the window)
	states, err = ewmh.WmStateGet(X, w)
	if err != nil {
		states = []string{}
	}
	if sticky && !common.IsInList("_NET_WM_STATE_STICKY", states) {
		states = append(states, "_NET_WM_STATE_STICKY")
	}

	// Window normal hints (normal hints of the window)
	nhints, err := icccm.WmNormalHintsGet(X, w)
	if err != nil {
		nhints = &icccm.NormalHints{}
	}

	// Window motif hints (hints of the window)
	mhints, err := motif.WmHintsGet(X, w)
	if err != nil {
		mhints = &motif.Hints{}
	}

	// Window extents (server/client decorations of the window)
	extNet, _ := xprop.PropValNums(xprop.GetProperty(X, w, "_NET_FRAME_EXTENTS"))
	extGtk, _ := xprop.PropValNums(xprop.GetProperty(X, w, "_GTK_FRAME_EXTENTS"))

	ext := make([]uint, 4)
	for i, e := range extNet {
		ext[i] += e
	}
	for i, e := range extGtk {
		ext[i] -= e
	}

	// Window dimensions (geometry/extent information for move/resize)
	dimensions = Dimensions{
		Geometry: *common.CreateGeometry(geom),
		Hints: Hints{
			Normal: *nhints,
			Motif:  *mhints,
		},
		Extents: ewmh.FrameExtents{
			Left:   int(ext[0]),
			Right:  int(ext[1]),
			Top:    int(ext[2]),
			Bottom: int(ext[3]),
		},
		AdjPos:     (nhints.WinGravity > 1 && !common.AllZero(extNet)) || !common.AllZero(extGtk),
		AdjSize:    !common.AllZero(extNet) || !common.AllZero(extGtk),
		AdjRestore: !common.AllZero(extGtk),
	}

	return &Info{
		Class:      class,
		Name:       name,
		Types:      types,
		States:     states,
		Location:   location,
		Dimensions: dimensions,
	}
}
