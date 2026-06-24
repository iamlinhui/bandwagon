package dashboard

import (
	"context"
	"log"
	"sync"
	"time"

	"bandwagon/internal/kiwivm"
)

// Instance is the serializable snapshot of one VPS, served to the UI.
// It intentionally contains no secrets (the API key lives only in config).
type Instance struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Group        string    `json:"group"`
	VEID         string    `json:"veid"`
	Online       bool      `json:"online"`
	Status       string    `json:"status"`
	Error        string    `json:"error,omitempty"`
	IPAddresses  []string  `json:"ip_addresses"`
	OS           string    `json:"os"`
	Hostname     string    `json:"hostname"`
	NodeAlias    string    `json:"node_alias"`
	Location     string    `json:"location"`
	Plan         string    `json:"plan"`
	SSHPort      int       `json:"ssh_port"`
	RAMTotal     int64     `json:"ram_total"`
	RAMUsed      int64     `json:"ram_used"`
	DiskTotal    int64     `json:"disk_total"`
	DiskUsed     int64     `json:"disk_used"`
	SwapTotal    int64     `json:"swap_total"`
	TrafficUsed  int64     `json:"traffic_used"`
	TrafficTotal int64     `json:"traffic_total"`
	TrafficReset int64     `json:"traffic_reset"`
	Uptime       int64     `json:"uptime"`
	Suspended    bool      `json:"suspended"`
	LoadAverage  []float64 `json:"load_average"`
	UpdatedAt    int64     `json:"updated_at"`
}

// Snapshot is the aggregate payload returned by GET /api/instances.
type Snapshot struct {
	Instances   []Instance `json:"instances"`
	UpdatedAt   int64      `json:"updated_at"`
	PollSeconds int        `json:"poll_seconds"`
	Title       string     `json:"title"`
	AuthEnabled bool       `json:"auth_enabled"`
}

// Store holds the latest per-instance snapshots and refreshes them in the
// background by polling the KiwiVM API.
type Store struct {
	mu        sync.RWMutex
	byID      map[string]*Instance
	order     []string
	cfgByVEID map[string]InstanceConfig
	client    *kiwivm.Client
	pollSec   int
	title     string
	updatedAt int64
}

// NewStore seeds the store with placeholders and starts the background poller.
func NewStore(cfg []InstanceConfig, pollSec int, title string) *Store {
	s := &Store{
		byID:      make(map[string]*Instance, len(cfg)),
		cfgByVEID: make(map[string]InstanceConfig, len(cfg)),
		client:    kiwivm.NewClient(),
		pollSec:   pollSec,
		title:     title,
	}
	for _, ic := range cfg {
		inst := &Instance{
			ID:     ic.ID,
			Name:   ic.Name,
			Group:  ic.Group,
			VEID:   ic.VEID,
			Status: "pending",
		}
		s.byID[ic.ID] = inst
		s.order = append(s.order, ic.ID)
		s.cfgByVEID[ic.VEID] = ic
	}
	return s
}

// Run blocks until ctx is cancelled, polling the API every poll interval.
// It does an immediate first poll so the dashboard is populated on startup.
func (s *Store) Run(ctx context.Context) {
	s.pollOnce()
	ticker := time.NewTicker(time.Duration(s.pollSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollOnce()
		}
	}
}

func (s *Store) pollOnce() {
	for i, id := range s.order {
		cfg := s.cfgByVEID[s.byID[id].VEID]
		if i > 0 {
			// Pace requests: KiwiVM rate-limits aggressive callers.
			time.Sleep(900 * time.Millisecond)
		}
		s.refreshOne(cfg)
	}
	s.mu.Lock()
	s.updatedAt = time.Now().Unix()
	s.mu.Unlock()
}

// refreshOne fetches service + live data for a single VPS and stores a snapshot.
func (s *Store) refreshOne(cfg InstanceConfig) {
	inst := &Instance{
		ID:    cfg.ID,
		Name:  cfg.Name,
		Group: cfg.Group,
		VEID:  cfg.VEID,
	}

	svc, err := s.client.GetServiceInfo(cfg.VEID, cfg.APIKey)
	if err != nil {
		inst.Status = "error"
		inst.Online = false
		inst.Error = kiwivm.FriendlyErr(err)
		inst.UpdatedAt = time.Now().Unix()
		s.set(inst)
		log.Printf("instance %s (%s): %v", cfg.Name, cfg.VEID, err)
		return
	}

	inst.IPAddresses = svc.IPAddresses
	inst.OS = svc.OS
	inst.Hostname = svc.Hostname
	inst.NodeAlias = svc.NodeAlias
	inst.Location = svc.Location
	inst.Plan = svc.Plan
	inst.RAMTotal = svc.PlanRAM
	inst.DiskTotal = svc.PlanDisk
	inst.SwapTotal = svc.PlanSwap
	inst.TrafficUsed = svc.DataCounter
	inst.TrafficTotal = svc.PlanMonthlyData
	inst.TrafficReset = svc.DataNextReset
	inst.Suspended = svc.Suspended

	// Live info is best-effort and slower; it supplies ve_status, ssh_port,
	// load, used-disk and available-memory — none of which getServiceInfo
	// returns. Don't fail the whole card if it can't be reached.
	if live, err := s.client.GetLiveInfo(cfg.VEID, cfg.APIKey); err == nil {
		inst.SSHPort = live.SSHPort
		inst.Uptime = live.VEUptime
		inst.LoadAverage = kiwivm.ParseLoadAverage(live.LoadAverage)
		inst.DiskUsed = live.VEUsedDiskSpace
		// Available RAM → used = total − available (KiwiVM reports KB).
		if live.MemAvailableKB > 0 && inst.RAMTotal > 0 {
			if used := inst.RAMTotal - live.MemAvailableKB*1024; used >= 0 {
				inst.RAMUsed = used
			}
		}
		if live.VEStatus != "" {
			inst.Status = live.VEStatus
			inst.Online = live.VEStatus == "running" && !inst.Suspended
		}
	}

	inst.UpdatedAt = time.Now().Unix()
	s.set(inst)
}

func (s *Store) set(inst *Instance) {
	s.mu.Lock()
	s.byID[inst.ID] = inst
	s.mu.Unlock()
}

// Snapshot returns the current aggregate state for the UI.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Snapshot{
		Instances:   make([]Instance, 0, len(s.order)),
		UpdatedAt:   s.updatedAt,
		PollSeconds: s.pollSec,
		Title:       s.title,
	}
	for _, id := range s.order {
		if inst, ok := s.byID[id]; ok {
			clone := *inst
			out.Instances = append(out.Instances, clone)
		}
	}
	return out
}

// configFor returns the config (including key) for an instance by id, for
// server-side power actions.
func (s *Store) configFor(id string) (InstanceConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if inst, ok := s.byID[id]; ok {
		cfg, ok := s.cfgByVEID[inst.VEID]
		return cfg, ok
	}
	return InstanceConfig{}, false
}
