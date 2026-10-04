package sms

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bethropolis/kcd/internal/config"
	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/internal/log"
	"github.com/bethropolis/kcd/internal/protocol"
)

// syncCaptureSender is thread-safe: arming sends from a goroutine so the
// send races the test goroutine's read of the recorded packets.
type syncCaptureSender struct {
	captureSender
	mu   sync.Mutex
	sent []*protocol.Packet
}

func (s *syncCaptureSender) Send(p *protocol.Packet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, p)
	return nil
}

func (s *syncCaptureSender) types() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sent))
	for _, p := range s.sent {
		out = append(out, p.Type)
	}
	return out
}

func (s *syncCaptureSender) count(t string) int {
	n := 0
	for _, got := range s.types() {
		if got == t {
			n++
		}
	}
	return n
}

// armingSettled waits for the arming goroutine to drain.
func armingSettled(t *testing.T, dev *syncCaptureSender) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dev.count(PacketTypeSMSRequestConvs) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newArmingPlugin(t *testing.T, cfg config.SMSConfig) (*SMSPlugin, *events.Bus) {
	t.Helper()
	bus := events.NewBus(log.Nop())
	return NewSMSPlugin(cfg, bus, nil, log.Nop()), bus
}

// Opting in arms the phone on connect.
func TestOnConnectArmsWhenAlwaysArmSet(t *testing.T) {
	cfg := config.SMSConfig{AlwaysArm: true}
	p, _ := newArmingPlugin(t, cfg)
	dev := &syncCaptureSender{}

	p.OnConnect(dev)
	armingSettled(t, dev)

	if got := dev.count(PacketTypeSMSRequestConvs); got != 1 {
		t.Fatalf("request_conversations sent %d times, want 1", got)
	}
}

// The shipped default must not ask the phone for anything: an armed phone
// cannot be un-armed, so opting in has to be deliberate.
func TestDefaultsDoNotArm(t *testing.T) {
	cfg := config.SMSConfig{}
	cfg.Defaults()
	if cfg.AlwaysArm {
		t.Fatal("SMSConfig.Defaults sets AlwaysArm; an armed phone cannot be un-armed")
	}

	p, _ := newArmingPlugin(t, cfg)
	dev := &syncCaptureSender{}
	p.OnConnect(dev)
	time.Sleep(100 * time.Millisecond)

	if got := len(dev.types()); got != 0 {
		t.Fatalf("default config sent %v, want nothing", dev.types())
	}
}

// With always_arm off and nobody listening, the phone must not be asked.
func TestNoArmWithoutSubscribers(t *testing.T) {
	p, _ := newArmingPlugin(t, config.SMSConfig{AlwaysArm: false})
	dev := &syncCaptureSender{}

	p.OnConnect(dev)
	time.Sleep(100 * time.Millisecond)

	if got := len(dev.types()); got != 0 {
		t.Fatalf("sent %v with no subscribers, want none", dev.types())
	}
}

// With always_arm off, a client watching sms.incoming is the opt-in.
func TestArmsWhenSubscribed(t *testing.T) {
	p, bus := newArmingPlugin(t, config.SMSConfig{AlwaysArm: false})
	dev := &syncCaptureSender{}
	p.OnConnect(dev)
	time.Sleep(50 * time.Millisecond)

	sub := bus.Subscribe(1, events.TypeSMSIncoming)
	defer sub.Close()
	armingSettled(t, dev)

	if got := dev.count(PacketTypeSMSRequestConvs); got != 1 {
		t.Fatalf("request_conversations sent %d times after subscribe, want 1", got)
	}
}

// The bus hook fires on every subscribe of any event type. Arming is
// idempotent per connection so a reconnecting watcher cannot re-trigger a
// conversation-head burst each time.
func TestArmsOnlyOncePerConnection(t *testing.T) {
	cfg := config.SMSConfig{AlwaysArm: true}
	p, bus := newArmingPlugin(t, cfg)
	dev := &syncCaptureSender{}

	p.OnConnect(dev)
	armingSettled(t, dev)

	for range 5 {
		s := bus.Subscribe(1, events.TypeBatteryUpdate)
		s.Close()
	}
	time.Sleep(100 * time.Millisecond)

	if got := dev.count(PacketTypeSMSRequestConvs); got != 1 {
		t.Fatalf("request_conversations sent %d times, want 1", got)
	}
}

func TestOnDisconnectForgetsDevice(t *testing.T) {
	cfg := config.SMSConfig{AlwaysArm: true}
	p, _ := newArmingPlugin(t, cfg)
	dev := &syncCaptureSender{}

	p.OnConnect(dev)
	armingSettled(t, dev)
	p.OnDisconnect(dev)

	p.mu.Lock()
	_, stillArmed := p.armedAt[dev.ID()]
	p.mu.Unlock()
	if stillArmed {
		t.Error("device still marked armed after disconnect")
	}
}

// The phone's content observer has no empty guard, so empty batches are
// routine once armed and must not produce events.
func TestEmptyBatchPublishesNothing(t *testing.T) {
	cfg := config.SMSConfig{NotifyIncoming: true}
	p, bus := newArmingPlugin(t, cfg)
	sub := bus.Subscribe(4, events.TypeSMSIncoming)
	defer sub.Close()

	pkt, err := protocol.NewPacket(PacketTypeSMSMessages, map[string]any{
		"version":  2,
		"messages": []SMSMessage{},
	})
	if err != nil {
		t.Fatalf("NewPacket: %v", err)
	}
	if err := p.Handle(context.Background(), &syncCaptureSender{}, pkt); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	select {
	case ev := <-sub.C:
		t.Fatalf("unexpected event for empty batch: %+v", ev)
	default:
	}
}

// Messages that predate the arm are the reply burst, not new mail.
func TestShouldNotifyDropsArmingBurst(t *testing.T) {
	cfg := config.SMSConfig{NotifyIncoming: true}
	p, _ := newArmingPlugin(t, cfg)

	armAt := time.Now()
	armAtMs := armAt.UnixMilli()

	old := SMSMessage{Body: "history", Type: 1, Date: armAtMs - 60_000}
	if p.shouldNotify(old, true, armAtMs) {
		t.Error("notified for a message older than the arm time")
	}

	fresh := SMSMessage{Body: "hello", Type: 1, Date: armAtMs + 1}
	if !p.shouldNotify(fresh, true, armAtMs) {
		t.Error("did not notify for a message newer than the arm time")
	}
}

// The phone cannot be un-armed, so packets keep arriving after every
// client leaves. Notifications must stop regardless.
func TestShouldNotifySilentWhenUnarmed(t *testing.T) {
	cfg := config.SMSConfig{NotifyIncoming: true}
	p, _ := newArmingPlugin(t, cfg)

	fresh := SMSMessage{Body: "hello", Type: 1, Date: time.Now().UnixMilli() + 1}
	if p.shouldNotify(fresh, false, 0) {
		t.Error("notified while no client is watching")
	}
}

// Outbound messages come back in the same batch and must not notify.
func TestShouldNotifyIgnoresOutbound(t *testing.T) {
	cfg := config.SMSConfig{NotifyIncoming: true}
	p, _ := newArmingPlugin(t, cfg)

	outbound := SMSMessage{Body: "sent", Type: 2, Date: time.Now().UnixMilli() + 1}
	if p.shouldNotify(outbound, true, 0) {
		t.Error("notified for an outbound message")
	}
}

func TestShouldNotifyRespectsConfig(t *testing.T) {
	p, _ := newArmingPlugin(t, config.SMSConfig{AlwaysArm: true, NotifyIncoming: false})
	if p.shouldNotify(SMSMessage{Type: 1, Date: 1 << 40}, true, 0) {
		t.Error("notified with notify_incoming disabled")
	}
}

// Journals get collected and shipped off-box, so a message body must never
// reach the logger. Only the bus and notify-send may carry it.
func TestMessageBodyNeverLogged(t *testing.T) {
	const secret = "my bank code is 1234"
	logger, snapshot := log.Observe()
	bus := events.NewBus(log.Nop())
	// PublishIncoming so the event assertion below has something to read; the
	// point of this test is that the body reaches neither the log nor, by
	// default, the bus.
	p := NewSMSPlugin(config.SMSConfig{AlwaysArm: true, NotifyIncoming: false, PublishIncoming: true}, bus, nil, logger)
	sub := bus.Subscribe(4, events.TypeSMSIncoming)
	defer sub.Close()

	pkt, err := protocol.NewPacket(PacketTypeSMSMessages, map[string]any{
		"version": 2,
		"messages": []SMSMessage{{
			Body:      secret,
			Type:      1,
			Date:      time.Now().UnixMilli(),
			ThreadID:  7,
			Addresses: []SMSAddress{{Address: "+15550100"}},
		}},
	})
	if err != nil {
		t.Fatalf("NewPacket: %v", err)
	}
	if err := p.Handle(context.Background(), &syncCaptureSender{}, pkt); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	entries := snapshot()
	if len(entries) == 0 {
		t.Fatal("no log entries captured; the assertion would be vacuous")
	}
	for _, e := range entries {
		if strings.Contains(e, secret) {
			t.Errorf("message body reached the log: %q", e)
		}
	}

	// The body must still reach the event, or the feature is broken.
	select {
	case ev := <-sub.C:
		payload, _ := ev.Payload.(map[string]any)
		if payload["body"] != secret {
			t.Errorf("event body = %v, want the message", payload["body"])
		}
	default:
		t.Fatal("no sms.incoming event published")
	}
}
