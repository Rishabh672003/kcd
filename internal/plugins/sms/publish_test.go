package sms

import (
	"context"
	"testing"
	"time"

	"github.com/bethropolis/kcd/internal/config"
	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/internal/log"
	"github.com/bethropolis/kcd/internal/protocol"
)

func pushOneMessage(t *testing.T, p *SMSPlugin) {
	t.Helper()
	pkt, err := protocol.NewPacket(PacketTypeSMSMessages, map[string]any{
		"version": 2,
		"messages": []SMSMessage{{
			Body:      "Yes",
			Addresses: []SMSAddress{{Address: "707"}},
			Date:      time.Now().UnixMilli(),
			Type:      1,
			ThreadID:  32,
		}},
	})
	if err != nil {
		t.Fatalf("NewPacket: %v", err)
	}
	if err := p.Handle(context.Background(), &syncCaptureSender{}, pkt); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestMessageNotPublishedByDefault(t *testing.T) {
	p, bus := newArmingPlugin(t, config.SMSConfig{})
	sub := bus.Subscribe(4, events.TypeSMSIncoming)
	defer sub.Close()

	pushOneMessage(t, p)

	select {
	case ev := <-sub.C:
		t.Fatalf("published %+v with publish_incoming unset; body would leak to any watcher", ev)
	default:
	}
}

func TestMessagePublishedWhenOptedIn(t *testing.T) {
	p, bus := newArmingPlugin(t, config.SMSConfig{PublishIncoming: true})
	sub := bus.Subscribe(4, events.TypeSMSIncoming)
	defer sub.Close()

	pushOneMessage(t, p)

	select {
	case ev := <-sub.C:
		body, _ := ev.Payload.(map[string]any)["body"].(string)
		if body != "Yes" {
			t.Fatalf("body = %q, want %q", body, "Yes")
		}
	case <-time.After(time.Second):
		t.Fatal("no event published with publish_incoming = true")
	}
}

func TestNotifyWithoutPublishKeepsBodyOffTheBus(t *testing.T) {
	p, bus := newArmingPlugin(t, config.SMSConfig{NotifyIncoming: true})
	sub := bus.Subscribe(4, events.TypeSMSIncoming)
	defer sub.Close()

	// Arm the way an operator would, so the notification path is live.
	dev := &syncCaptureSender{}
	p.OnConnect(dev)
	sub.Close()
	sub = bus.Subscribe(4, events.TypeSMSIncoming)
	defer sub.Close()

	pushOneMessage(t, p)

	select {
	case ev := <-sub.C:
		t.Fatalf("notify_incoming alone published %+v to the bus", ev)
	default:
	}
}

func TestDefaultsDoNotPublishIncoming(t *testing.T) {
	var cfg config.SMSConfig
	cfg.Defaults()
	if cfg.PublishIncoming {
		t.Fatal("SMSConfig.Defaults sets PublishIncoming; message bodies would flow without being asked for")
	}
	if cfg.AlwaysArm {
		t.Fatal("SMSConfig.Defaults sets AlwaysArm; an armed phone cannot be un-armed")
	}
}

func TestUnfilteredSubscriberDoesNotMatchSMS(t *testing.T) {
	bus := events.NewBus(log.Nop())
	sub := bus.Subscribe(1, events.AllExceptOptIn()...)
	defer sub.Close()

	for _, typ := range events.OptInOnly() {
		if bus.HasSubscribers(typ) {
			t.Errorf("an SMS subscriber is present for %q; a bare watch would arm the phone", typ)
		}
	}
	// Everything else must still work.
	for _, typ := range events.All() {
		if isOptIn(typ) {
			continue
		}
		if !bus.HasSubscribers(typ) {
			t.Errorf("HasSubscribers(%q) = false, want true", typ)
		}
	}
}

func isOptIn(t events.EventType) bool {
	for _, o := range events.OptInOnly() {
		if o == t {
			return true
		}
	}
	return false
}
