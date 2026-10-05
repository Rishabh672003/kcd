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

// Naming sms.incoming is the opt-in: it must both arm the phone and deliver
// the push. This is the whole consent path in one test.
func TestNamingSMSDeliversAndArms(t *testing.T) {
	p, bus := newArmingPlugin(t, config.SMSConfig{})
	dev := &syncCaptureSender{}
	p.OnConnect(dev)

	// Unfiltered: matches everything, which is the shape a bare watch used to
	// have. It must not deliver SMS.
	unfiltered := bus.Subscribe(1)
	pushOneMessage(t, p)
	select {
	case ev := <-unfiltered.C:
		t.Errorf("unfiltered subscriber received %+v", ev)
	default:
	}
	if bus.HasSubscribers(events.TypeSMSIncoming) {
		t.Error("an unfiltered subscriber armed the phone")
	}
	unfiltered.Close()

	sub := bus.Subscribe(1, events.TypeSMSIncoming)
	defer sub.Close()
	armingSettled(t, dev)

	if got := dev.count(PacketTypeSMSRequestConvs); got != 1 {
		t.Fatalf("naming sms.incoming sent %d arming requests, want 1", got)
	}

	pushOneMessage(t, p)
	select {
	case ev := <-sub.C:
		body, _ := ev.Payload.(map[string]any)["body"].(string)
		if body != "Yes" {
			t.Fatalf("body = %q, want %q", body, "Yes")
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered to a subscriber that named sms.incoming")
	}
}

// An unfiltered subscriber must not match SMS. This is what keeps a bare
// `kcd watch` from arming the phone.
func TestUnfilteredSubscriberDoesNotMatchSMS(t *testing.T) {
	bus := events.NewBus(log.Nop())
	sub := bus.Subscribe(1)
	defer sub.Close()

	for _, typ := range events.OptInOnly() {
		if bus.HasSubscribers(typ) {
			t.Errorf("subscriber matches %q; a bare watch would arm the phone", typ)
		}
	}
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
