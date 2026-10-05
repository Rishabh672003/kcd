package sms

import (
	"time"

	"github.com/bethropolis/kcd/internal/device"
	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/internal/log"
	"github.com/bethropolis/kcd/internal/protocol"
)

// --- SMS sending -----------------------------------------------------------

func (p *SMSPlugin) SendSMS(dev device.Sender, phoneNumber, message string) error {
	// v2 schema: the phone reads only messageBody, with addresses as the
	// primary recipient list (phoneNumber stays as a legacy fallback for
	// older peers). Without addresses/version the phone sends a blank SMS.
	body := map[string]any{
		"version":     2,
		"addresses":   []map[string]string{{"address": phoneNumber}},
		"messageBody": message,
		"phoneNumber": phoneNumber,
	}
	pkt, err := protocol.NewPacket(PacketTypeSMSRequest, body)
	if err != nil {
		return err
	}
	return dev.Send(pkt)
}

// --- Conversation browsing ------------------------------------------------

// RequestConversations asks the phone for a summary of all conversations.
// Bodyless requests use an empty object (never null) on the wire; see
// contacts.RequestSync for why explicit null is dangerous.
func (p *SMSPlugin) RequestConversations(dev device.Sender) error {
	pkt, err := protocol.NewPacket(PacketTypeSMSRequestConvs, map[string]any{})
	if err != nil {
		return err
	}
	return dev.Send(pkt)
}

// RequestConversation asks the phone for messages in a specific thread.
// Pass -1 for rangeStartTimestamp or numberToRequest for no limit.
func (p *SMSPlugin) RequestConversation(dev device.Sender, threadID int64, rangeStartTimestamp int64, numberToRequest int64) error {
	body := map[string]any{
		"threadID": threadID,
	}
	if rangeStartTimestamp >= 0 {
		body["rangeStartTimestamp"] = rangeStartTimestamp
	}
	if numberToRequest >= 0 {
		body["numberToRequest"] = numberToRequest
	}
	pkt, err := protocol.NewPacket(PacketTypeSMSRequestConv, body)
	if err != nil {
		return err
	}
	return dev.Send(pkt)
}

// RequestAttachment asks the phone to send an MMS attachment file.
func (p *SMSPlugin) RequestAttachment(dev device.Sender, partID int64, uniqueIdentifier string) error {
	body := map[string]any{
		"part_id":           partID,
		"unique_identifier": uniqueIdentifier,
	}
	pkt, err := protocol.NewPacket(PacketTypeSMSRequestAtt, body)
	if err != nil {
		return err
	}
	return dev.Send(pkt)
}

// --- Arming ------------------------------------------------------------------

// armed reports whether the phone should be streaming new SMS to us. The
// phone suppresses every push until it has been asked once, so this is the
// single gate. It is off by default: an armed phone cannot be un-armed.
func (p *SMSPlugin) armed() bool {
	if p.cfg.AlwaysArm {
		return true
	}
	return p.bus != nil && p.bus.HasSubscribers(events.TypeSMSIncoming)
}

// syncArming arms every connected device that is not armed yet. It runs
// from the bus subscriber-change hook, which must return quickly, so the
// device sends happen in a goroutine: dev.Send blocks for up to its write
// timeout when the peer's send channel is full, and a hook that blocked
// would stall bus.Subscribe for every caller.
func (p *SMSPlugin) syncArming() {
	if !p.armed() {
		return
	}

	p.mu.Lock()
	targets := make([]device.Sender, 0, len(p.devices))
	now := time.Now()
	for id, dev := range p.devices {
		if _, ok := p.armedAt[id]; ok {
			continue
		}
		p.armedAt[id] = now
		targets = append(targets, dev)
	}
	p.mu.Unlock()

	if len(targets) == 0 {
		return
	}

	go func() {
		for _, dev := range targets {
			if !dev.IsConnected() {
				continue
			}
			if !dev.HasCapability(PacketTypeSMSRequestConvs) {
				continue
			}
			if err := p.RequestConversations(dev); err != nil {
				p.logger.Warn("sms: failed to arm push", log.Error(err),
					log.String("device_id", dev.ID()))
				continue
			}
			p.logger.Debug("sms: armed push", log.String("device_id", dev.ID()))
		}
	}()
}

// --- Lifecycle ---------------------------------------------------------------

// OnConnect tracks the device and re-arms it. The phone's arm flag survives
// a reconnect, but not a restart of its own app, so re-arming per connection
// is the only recovery available over this protocol.
func (p *SMSPlugin) OnConnect(dev device.Sender) {
	p.mu.Lock()
	p.devices[dev.ID()] = dev
	delete(p.armedAt, dev.ID())
	p.mu.Unlock()

	p.syncArming()
}

// OnDisconnect forgets the device so the next connect re-arms it.
func (p *SMSPlugin) OnDisconnect(dev device.Sender) {
	p.mu.Lock()
	delete(p.devices, dev.ID())
	delete(p.armedAt, dev.ID())
	p.mu.Unlock()
}
