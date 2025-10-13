package store

import (
	"fmt"
	"math"
	"sync"

	"github.com/leukipp/cortile/v2/common"

	log "github.com/sirupsen/logrus"
)

type Manager struct {
	Name        string       // Manager name with window clients
	Location    *Location    // Manager workspace and screen location
	Proportions *Proportions // Manager proportions of window clients
	Masters     *Clients     // List of master window clients
	Slaves      *Clients     // List of slave window clients
	Decoration  bool         // Window decoration is enabled
	mu          sync.RWMutex // Protects all mutable fields
}

type Location struct {
	Desktop uint // Location desktop index
	Screen  uint // Location screen index
}

type Proportions struct {
	MasterSlave  map[int][]float64 // Master-slave proportions
	MasterMaster map[int][]float64 // Master-master proportions
	SlaveSlave   map[int][]float64 // Slave-slave proportions
}

type Clients struct {
	Maximum int       // Currently maximum allowed clients
	Stacked []*Client `json:"-"` // List of stored window clients
}

type Directions struct {
	Top    bool // Indicates proportion changes on the top
	Right  bool // Indicates proportion changes on the right
	Bottom bool // Indicates proportion changes on the bottom
	Left   bool // Indicates proportion changes on the left
}

const (
	Stacked uint8 = 1 // Flag for stacked (internal index order) clients
	Ordered uint8 = 2 // Flag for ordered (bottom to top order) clients
	Visible uint8 = 3 // Flag for visible (top only) clients
)

// SerializableManager contains only the data needed for JSON serialization
type SerializableManager struct {
	Location    Location
	Proportions struct {
		MasterSlave  map[int][]float64
		MasterMaster map[int][]float64
		SlaveSlave   map[int][]float64
	}
	MastersMaximum int
	SlavesMaximum  int
	Decoration     bool
}

func CreateManager(loc Location) *Manager {
	return &Manager{
		Name:     fmt.Sprintf("manager-%d-%d", loc.Desktop, loc.Screen),
		Location: &loc,
		Proportions: &Proportions{
			MasterSlave:  calcProportions(2),
			MasterMaster: calcProportions(common.Config.WindowMastersMax),
			SlaveSlave:   calcProportions(common.Config.WindowSlavesMax),
		},
		Masters: &Clients{
			Maximum: 1,
			Stacked: make([]*Client, 0),
		},
		Slaves: &Clients{
			Maximum: common.Config.WindowSlavesMax,
			Stacked: make([]*Client, 0),
		},
		Decoration: common.Config.WindowDecoration,
	}
}

// GetSerializable returns a deep copy of serializable fields under lock protection
func (mg *Manager) GetSerializable() SerializableManager {
	mg.mu.RLock()
	defer mg.mu.RUnlock()

	snapshot := SerializableManager{
		Location:       *mg.Location,
		MastersMaximum: mg.Masters.Maximum,
		SlavesMaximum:  mg.Slaves.Maximum,
		Decoration:     mg.Decoration,
	}

	// Deep copy Proportions maps
	snapshot.Proportions.MasterSlave = make(map[int][]float64, len(mg.Proportions.MasterSlave))
	for k, v := range mg.Proportions.MasterSlave {
		snapshot.Proportions.MasterSlave[k] = append([]float64(nil), v...)
	}
	snapshot.Proportions.MasterMaster = make(map[int][]float64, len(mg.Proportions.MasterMaster))
	for k, v := range mg.Proportions.MasterMaster {
		snapshot.Proportions.MasterMaster[k] = append([]float64(nil), v...)
	}
	snapshot.Proportions.SlaveSlave = make(map[int][]float64, len(mg.Proportions.SlaveSlave))
	for k, v := range mg.Proportions.SlaveSlave {
		snapshot.Proportions.SlaveSlave[k] = append([]float64(nil), v...)
	}

	return snapshot
}

func (mg *Manager) EnableDecoration() {
	mg.Decoration = true
}

func (mg *Manager) DisableDecoration() {
	mg.Decoration = false
}

func (mg *Manager) DecorationEnabled() bool {
	return mg.Decoration
}

func (mg *Manager) DecorationDisabled() bool {
	return !mg.Decoration
}

func (mg *Manager) AddClient(c *Client) {
	mg.mu.Lock()
	defer mg.mu.Unlock()

	if mg.isMaster(c) || mg.isSlave(c) {
		return
	}

	log.Debug("Add client for manager [", c.GetLatest().Class, ", ", mg.Name, "]")

	if len(mg.Masters.Stacked) < mg.Masters.Maximum {
		mg.Masters.Stacked = addClient(mg.Masters.Stacked, c)
	} else {
		mg.Slaves.Stacked = addClient(mg.Slaves.Stacked, c)
	}
}

func (mg *Manager) RemoveClient(c *Client) {
	mg.mu.Lock()
	defer mg.mu.Unlock()

	log.Debug("Remove client from manager [", c.GetLatest().Class, ", ", mg.Name, "]")

	// Remove master window
	mi := mg.index(mg.Masters, c)
	if mi >= 0 {
		if len(mg.Slaves.Stacked) > 0 {
			mg.swapClient(mg.Masters.Stacked[mi], mg.Slaves.Stacked[0])
			mg.Slaves.Stacked = mg.Slaves.Stacked[1:]
		} else {
			mg.Masters.Stacked = removeClient(mg.Masters.Stacked, mi)
		}
	}

	// Remove slave window
	si := mg.index(mg.Slaves, c)
	if si >= 0 {
		mg.Slaves.Stacked = removeClient(mg.Slaves.Stacked, si)
	}
}

func (mg *Manager) MakeMaster(c *Client) {
	mg.mu.Lock()
	defer mg.mu.Unlock()

	log.Info("Make window master [", c.GetLatest().Class, ", ", mg.Name, "]")

	if len(mg.Masters.Stacked) > 0 {
		mg.swapClient(c, mg.Masters.Stacked[0])
	}
}

func (mg *Manager) SwapClient(c1 *Client, c2 *Client) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	mg.swapClient(c1, c2)
}

// swapClient is the internal version that assumes lock is held
func (mg *Manager) swapClient(c1 *Client, c2 *Client) {
	log.Info("Swap clients [", c1.GetLatest().Class, "-", c2.GetLatest().Class, ", ", mg.Name, "]")

	mIndex1 := mg.index(mg.Masters, c1)
	sIndex1 := mg.index(mg.Slaves, c1)

	mIndex2 := mg.index(mg.Masters, c2)
	sIndex2 := mg.index(mg.Slaves, c2)

	// Swap master with master
	if mIndex1 >= 0 && mIndex2 >= 0 {
		mg.Masters.Stacked[mIndex2], mg.Masters.Stacked[mIndex1] = mg.Masters.Stacked[mIndex1], mg.Masters.Stacked[mIndex2]
		return
	}

	// Swap master with slave
	if mIndex1 >= 0 && sIndex2 >= 0 {
		mg.Slaves.Stacked[sIndex2], mg.Masters.Stacked[mIndex1] = mg.Masters.Stacked[mIndex1], mg.Slaves.Stacked[sIndex2]
		return
	}

	// Swap slave with master
	if sIndex1 >= 0 && mIndex2 >= 0 {
		mg.Masters.Stacked[mIndex2], mg.Slaves.Stacked[sIndex1] = mg.Slaves.Stacked[sIndex1], mg.Masters.Stacked[mIndex2]
		return
	}

	// Swap slave with slave
	if sIndex1 >= 0 && sIndex2 >= 0 {
		mg.Slaves.Stacked[sIndex2], mg.Slaves.Stacked[sIndex1] = mg.Slaves.Stacked[sIndex1], mg.Slaves.Stacked[sIndex2]
		return
	}
}

func (mg *Manager) ActiveClient() *Client {
	clients := mg.Clients(Stacked)

	// Get active client
	for _, c := range clients {
		if c.Window.Id == Windows.Active.Id {
			return c
		}
	}

	return nil
}

func (mg *Manager) NextClient() *Client {
	clients := mg.Clients(Stacked)
	last := len(clients) - 1

	// Get next window
	next := -1
	for i, c := range clients {
		if c.Window.Id == Windows.Active.Id {
			next = i + 1
			if next > last {
				next = 0
			}
			break
		}
	}

	// Invalid active window
	if next == -1 {
		return nil
	}

	return clients[next]
}

func (mg *Manager) PreviousClient() *Client {
	clients := mg.Clients(Stacked)
	last := len(clients) - 1

	// Get previous window
	prev := -1
	for i, c := range clients {
		if c.Window.Id == Windows.Active.Id {
			prev = i - 1
			if prev < 0 {
				prev = last
			}
			break
		}
	}

	// Invalid active window
	if prev == -1 {
		return nil
	}

	return clients[prev]
}

func (mg *Manager) IncreaseMaster() {
	mg.mu.Lock()
	defer mg.mu.Unlock()

	// Increase master area
	if len(mg.Slaves.Stacked) > 1 && mg.Masters.Maximum < common.Config.WindowMastersMax {
		mg.Masters.Maximum += 1
		mg.Masters.Stacked = append(mg.Masters.Stacked, mg.Slaves.Stacked[0])
		mg.Slaves.Stacked = mg.Slaves.Stacked[1:]
	}

	log.Info("Increase masters to ", mg.Masters.Maximum)
}

func (mg *Manager) DecreaseMaster() {
	mg.mu.Lock()
	defer mg.mu.Unlock()

	// Decrease master area
	if len(mg.Masters.Stacked) > 0 {
		mg.Masters.Maximum -= 1
		mg.Slaves.Stacked = append([]*Client{mg.Masters.Stacked[len(mg.Masters.Stacked)-1]}, mg.Slaves.Stacked...)
		mg.Masters.Stacked = mg.Masters.Stacked[:len(mg.Masters.Stacked)-1]
	}

	log.Info("Decrease masters to ", mg.Masters.Maximum)
}

func (mg *Manager) IncreaseSlave() {
	mg.mu.Lock()
	defer mg.mu.Unlock()

	// Increase slave area
	if mg.Slaves.Maximum < common.Config.WindowSlavesMax {
		mg.Slaves.Maximum += 1
	}

	log.Info("Increase slaves to ", mg.Slaves.Maximum)
}

func (mg *Manager) DecreaseSlave() {
	mg.mu.Lock()
	defer mg.mu.Unlock()

	// Decrease slave area
	if mg.Slaves.Maximum > 1 {
		mg.Slaves.Maximum -= 1
	}

	log.Info("Decrease slaves to ", mg.Slaves.Maximum)
}

func (mg *Manager) IncreaseProportion() {
	precision := 1.0 / common.Config.ProportionStep
	proportion := math.Round(mg.Proportions.MasterSlave[2][0]*precision)/precision + common.Config.ProportionStep

	// Increase root proportion
	mg.SetProportions(mg.Proportions.MasterSlave[2], proportion, 0, 1)
}

func (mg *Manager) DecreaseProportion() {
	precision := 1.0 / common.Config.ProportionStep
	proportion := math.Round(mg.Proportions.MasterSlave[2][0]*precision)/precision - common.Config.ProportionStep

	// Decrease root proportion
	mg.SetProportions(mg.Proportions.MasterSlave[2], proportion, 0, 1)
}

func (mg *Manager) SetProportions(ps []float64, pi float64, i int, j int) bool {
	mg.mu.Lock()
	defer mg.mu.Unlock()

	// Ignore changes on border sides
	if i == j || i < 0 || i >= len(ps) || j < 0 || j >= len(ps) {
		return false
	}

	// Clamp target proportion
	pic := math.Min(math.Max(pi, common.Config.ProportionMin), 1.0-common.Config.ProportionMin)
	if pi != pic {
		return false
	}

	// Clamp neighbor proportion
	pj := ps[j] + (ps[i] - pi)
	pjc := math.Min(math.Max(pj, common.Config.ProportionMin), 1.0-common.Config.ProportionMin)
	if pj != pjc {
		return false
	}

	// Update proportions
	ps[i] = pi
	ps[j] = pj

	return true
}

func (mg *Manager) IsMaster(c *Client) bool {
	mg.mu.RLock()
	defer mg.mu.RUnlock()
	return mg.isMaster(c)
}

func (mg *Manager) IsSlave(c *Client) bool {
	mg.mu.RLock()
	defer mg.mu.RUnlock()
	return mg.isSlave(c)
}

func (mg *Manager) Index(windows *Clients, c *Client) int {
	mg.mu.RLock()
	defer mg.mu.RUnlock()
	return mg.index(windows, c)
}

// isMaster is internal version that assumes lock is held
func (mg *Manager) isMaster(c *Client) bool {
	return mg.index(mg.Masters, c) >= 0
}

// isSlave is internal version that assumes lock is held
func (mg *Manager) isSlave(c *Client) bool {
	return mg.index(mg.Slaves, c) >= 0
}

// index is internal version that assumes lock is held
func (mg *Manager) index(windows *Clients, c *Client) int {
	for i, m := range windows.Stacked {
		if m.Window.Id == c.Window.Id {
			return i
		}
	}
	return -1
}

func (mg *Manager) Ordered(windows *Clients) []*Client {
	ordered := []*Client{}

	// Create ordered client list
	for _, w := range Windows.Stacked {
		for _, c := range windows.Stacked {
			if w.Id == c.Window.Id {
				ordered = append(ordered, c)
				break
			}
		}
	}

	return ordered
}

func (mg *Manager) Visible(windows *Clients) []*Client {
	visible := make([]*Client, common.MinInt(len(windows.Stacked), windows.Maximum))

	// Create visible client list
	for _, c := range mg.Ordered(windows) {
		visible[mg.Index(windows, c)%windows.Maximum] = c
	}

	return visible
}

func (mg *Manager) Clients(flag uint8) []*Client {
	mg.mu.RLock()
	defer mg.mu.RUnlock()

	switch flag {
	case Stacked:
		result := make([]*Client, len(mg.Masters.Stacked)+len(mg.Slaves.Stacked))
		copy(result, mg.Masters.Stacked)
		copy(result[len(mg.Masters.Stacked):], mg.Slaves.Stacked)
		return result
	case Ordered:
		return append(mg.Ordered(mg.Masters), mg.Ordered(mg.Slaves)...)
	case Visible:
		return append(mg.Visible(mg.Masters), mg.Visible(mg.Slaves)...)
	}
	return make([]*Client, 0)
}

func addClient(cs []*Client, c *Client) []*Client {
	return append([]*Client{c}, cs...)
}

func removeClient(cs []*Client, i int) []*Client {
	return append(cs[:i], cs[i+1:]...)
}

func calcProportions(n int) map[int][]float64 {
	p := map[int][]float64{}
	for i := 1; i <= n; i++ {
		for j := 1; j <= i; j++ {
			p[i] = append(p[i], 1.0/float64(i))
		}
	}
	return p
}
