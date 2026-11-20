package dashboard

import (
	"fmt"
	"log"
	"net/http"
	"sync"

	"bandwagon/internal/webssh"
)

// maxSSHPerInstance caps how many browser terminals may be attached to one VPS at
// once. Each is a real SSH connection holding a PTY, so leaked tabs must not be
// able to pile up sessions on a machine.
const maxSSHPerInstance = 3

// sshGate counts live sessions per instance.
type sshGate struct {
	mu   sync.Mutex
	live map[string]int
}

func newSSHGate() *sshGate { return &sshGate{live: make(map[string]int)} }

// acquire reserves a slot for id; the returned release must always be called.
func (g *sshGate) acquire(id string) (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.live[id] >= maxSSHPerInstance {
		return func() {}, false
	}
	g.live[id]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.live[id]--
			if g.live[id] <= 0 {
				delete(g.live, id)
			}
		})
	}, true
}

// handleSSH upgrades GET /api/instances/{id}/ssh to a WebSocket and bridges it to
// a real SSH PTY on that VPS.
//
// Credentials never leave the server: the browser only exchanges keystrokes and
// screen bytes. Pre-flight failures are reported *inside* the WebSocket via
// webssh.FailWS, because a WebSocket client never gets to see an HTTP error body.
func (s *Server) handleSSH(w http.ResponseWriter, r *http.Request) {
	// Report pre-flight problems through the socket itself: a WebSocket client
	// never sees an HTTP error body.
	fail := func(msg string) { webssh.FailWS(w, r, msg, s.origins...) }

	id := r.PathValue("id")
	cfg, found := s.store.configFor(id)
	if !found {
		fail("实例不存在: " + id)
		return
	}
	if !cfg.SSH.Present() {
		fail("这台机器没有配置 SSH 凭据:在 config.json 的 instances[].ssh 里填 user + password(或 private_key),然后重启看板。")
		return
	}
	host, port, ok := s.store.sshEndpoint(id)
	if !ok {
		fail("暂时拿不到这台机器的地址:等采集完成(机器需要在线),或在配置里写死 ssh.host / ssh.port。")
		return
	}
	release, ok := s.sshGate.acquire(id)
	if !ok {
		fail(fmt.Sprintf("这台机器已经有 %d 个终端连着,先关掉一个再试。", maxSSHPerInstance))
		return
	}
	defer release()

	target := webssh.Target{
		Host:               host,
		Port:               port,
		User:               cfg.SSH.User,
		Password:           cfg.SSH.Password,
		PrivateKeyPEM:      cfg.SSH.PrivateKey,
		Passphrase:         cfg.SSH.Passphrase,
		HostKeyFingerprint: cfg.SSH.HostKeyFingerprint,
		Label:              cfg.Name,
	}
	opts := webssh.Options{Target: target, OriginPatterns: s.origins, Log: log.Printf}
	if err := webssh.ServeWS(w, r, opts); err != nil {
		log.Printf("ssh %s (%s): %v", cfg.Name, cfg.VEID, err)
	}
}
