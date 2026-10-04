package device

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/internal/log"
	"github.com/bethropolis/kcd/internal/protocol"
	"github.com/bethropolis/kcd/internal/transport"
)

// Device represents an active KDE Connect remote device.
type Device struct {
	id   string
	name string
	Type string

	IncomingCaps []string
	OutgoingCaps []string

	state  PairingState
	CertFP string

	lastSeen time.Time
	lastIP   net.IP // cached from last successful connection; survives Disconnect
	// lastPort pairs with lastIP as the dial target. It is the port the peer
	// advertised over the authenticated post-TLS identity exchange, so an
	// unauthenticated discovery packet can never redirect a paired auto-dial.
	// Zero means unknown, and callers fall back to the configured port.
	lastPort int

	// discoveryIP/discoveryPort is where the device last announced itself
	// (UDP/mDNS), even with no TCP connection ever opened to it. Used to dial
	// on explicit user request (`kcd pair <id>`) without auto-dialing strangers.
	discoveryIP   net.IP
	discoveryPort int

	// pairDialRequested is the one-shot dial trigger for `kcd pair <id>`,
	// consumed by the first discovery announcement so the request can be
	// delivered. Contrast pairIntentUntil, which is not consumed.
	pairDialRequested atomic.Bool

	// pairIntentUntil is the Unix-nano deadline until which an explicit pair
	// intent keeps a connection alive. Unlike pairDialRequested it survives
	// dial/connect cycles, until pairing starts, resolves, or pairDialIntentTTL
	// expires — so a slow phone-side accept cannot downgrade into an
	// ephemeral-close flap.
	pairIntentUntil atomic.Int64

	// lastDiscoveryDial is when onDeviceFound last spawned a dial for this
	// device. It throttles sighting-triggered redials so announcements
	// (or a spoofed broadcast storm) can't cause a dial per packet.
	lastDiscoveryDial time.Time

	// ephemeralDialed records that this device already got its one ephemeral
	// discovery dial for the current unpaired era. Such a dial lets a stranger
	// finish the TCP identity exchange -- so both sides list each other --
	// without staying connected; the next sighting closes the socket again.
	// Cleared when the device is explicitly unpaired or rejected. Paired
	// devices and pairing mode bypass it.
	ephemeralDialed bool

	conn      *transport.Conn
	sendChan  chan *protocol.Packet // buffered 32
	done      chan struct{}
	closeOnce sync.Once

	// lastConnect marks the last completed handshake; handshakes inside
	// reconnectCooldown are refused so duplicate bursts cannot starve a retry.
	lastConnect time.Time
	// lastSightedIP is the previous sighting, so only confirmed roams reset
	// the reconnect backoff (see NoteSighting).
	lastSightedIP net.IP
	BatteryCharge int
	IsCharging    bool

	// batterySeen marks that a kdeconnect.battery packet has arrived. Until
	// then the zero values above are not measurements and must not be
	// published, or a fresh pair reports a stable bogus 0% that no later
	// packet corrects at steady charge.
	batterySeen bool
	// lastBatteryAt is when the last battery packet arrived, so clients
	// can apply their own staleness rules (mirrors mediaAgeMs).
	lastBatteryAt time.Time

	mu sync.RWMutex

	// reconnecting is an atomic flag preventing multiple concurrent
	// auto-reconnect goroutines for this device.
	reconnecting atomic.Bool

	// reconnectWake nudges a parked auto-reconnect loop: discovery
	// sightings (peer provably alive) and unpair transitions. Buffered-1
	// so pokes never block the discovery listener; coalesced bursts mean
	// "check now", not N dials.
	reconnectWake chan struct{}

	// reconnectAttempt persists the auto-reconnect backoff counter across
	// disconnect cycles. A connection that flaps (drops shortly after a
	// successful dial) keeps the counter so the backoff escalates instead of
	// resetting to the 2s floor; a stable connection resets it on drop.
	reconnectAttempt int

	// connectStarted is when the most recent connection was established,
	// used to detect flaps (connections that die too quickly to count as
	// genuinely stable).
	connectStarted time.Time

	// pluginDispatch routes incoming packets to registered plugins
	pluginDispatch func(ctx context.Context, dev *Device, pkt *protocol.Packet) bool
	onConnect      func(dev *Device)
	onDisconnect   func(dev *Device)

	logger log.Logger
	bus    *events.Bus
}

// NewDevice creates a new disconnected device instance.
// New devices start as Unpaired (not Unknown) so listings are unambiguous.
func NewDevice(id, name, dtype string, logger log.Logger) *Device {
	return &Device{
		id:       id,
		name:     name,
		Type:     dtype,
		state:    StateUnpaired,
		sendChan: make(chan *protocol.Packet, 32),
		done:     make(chan struct{}),
		// A send on a nil channel blocks forever, so the select always takes
		// the default branch — which is what makes this nil-safe and what
		// lets tests use a zero-value Device.
		reconnectWake: make(chan struct{}, 1),
		logger:        logger.With(log.String("device_id", id)),
	}
}

func (d *Device) SetBus(bus *events.Bus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bus = bus
}

func (d *Device) ID() string {
	return d.id
}
func (d *Device) Name() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.name
}
func (d *Device) SetName(n string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.name = n
}
func (d *Device) State() PairingState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.state
}
func (d *Device) SetState(s PairingState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state = s
	// Wake a parked reconnect loop so unpair takes effect immediately
	// instead of at the next timer fire (or never, past the stale
	// horizon). The loop re-checks state on wake and exits.
	if s == StateUnpaired {
		d.PokeReconnect()
	}
}

// PokeReconnect nudges the auto-reconnect loop to re-check now (sighting
// arrived, or state changed). Non-blocking and nil-safe: bursts coalesce
// into a single wakeup.
func (d *Device) PokeReconnect() {
	select {
	case d.reconnectWake <- struct{}{}:
	default:
	}
}

// ReconnectWake exposes the wake channel for the auto-reconnect loop's
// select. A nil channel (zero-value Device) blocks forever — safe.
func (d *Device) ReconnectWake() <-chan struct{} {
	return d.reconnectWake
}

// Reconnecting reports whether an auto-reconnect goroutine is running.
func (d *Device) Reconnecting() bool {
	return d.reconnecting.Load()
}
func (d *Device) LastSeen() time.Time {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastSeen
}
func (d *Device) SetLastSeen(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastSeen = t
}
