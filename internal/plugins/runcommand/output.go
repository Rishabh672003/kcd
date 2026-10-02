package runcommand

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/bethropolis/kcd/internal/device"
	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/internal/log"
	"github.com/bethropolis/kcd/internal/protocol"
)

// PacketTypeOutput carries command execution results to the phone's in-app
// output card. Its presence is what makes the daemon advertise the streaming
// path rather than relying on a desktop notification, which Android's
// ReceiveNotificationsPlugin ignores unless the user explicitly enables it.
const PacketTypeOutput = "kdeconnect.runcommand.output"

const (
	// streamFlushInterval bounds how long a line waits before the phone sees
	// it. It is the only knob trading latency against packet count: a chatty
	// command emitting thousands of lines would otherwise produce one packet
	// per line.
	streamFlushInterval = 250 * time.Millisecond

	// streamMaxLines caps a single execution's output. The phone keeps every
	// line in an unbounded Compose list, so the bound has to live here.
	streamMaxLines = 2000

	// streamMaxLineLen truncates a single pathological line (a minified
	// blob, a base64 dump) before it reaches the wire.
	streamMaxLineLen = 1024

	// streamChanBuffer absorbs a burst between flushes.
	streamChanBuffer = 256

	// streamMaxScanBuffer is the largest single line the scanner will read.
	// Anything longer is truncated by add rather than failing the scan.
	streamMaxScanBuffer = 1024 * 1024

	// streamNotifMaxBytes bounds the notification transcript. It is separate
	// from the streamed cap because a notification is a summary, not a log.
	streamNotifMaxBytes = 4000
)

// outputStream accumulates one execution's output and emits it as batched
// packets. Both stdout and stderr are sent on every batch, even when one is
// empty: the phone's handler iterates both lists without a null check.
type outputStream struct {
	mu      sync.Mutex
	dev     device.Sender
	logger  log.Logger
	bus     *events.Bus
	id      int32
	command string

	stdout []string
	stderr []string
	total  int
	// notif accumulates a combined transcript for the notification fallback,
	// which has a much smaller budget than the streamed output.
	notif      []string
	notifBytes int
	// dropped records that the line cap was hit, so the final batch can say so
	// rather than silently ending early.
	dropped bool
	// announced records that the started event went out, so the paired
	// finished event is not published for an execution that never began.
	announced bool
	// stopped records that the execution was cancelled, so the finish packet
	// reports failure even if the process happened to exit zero.
	stopped bool
}

func (s *outputStream) add(isStderr bool, line string) {
	if len(line) > streamMaxLineLen {
		line = line[:streamMaxLineLen] + "..."
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.total >= streamMaxLines {
		s.dropped = true
		return
	}
	s.total++
	if isStderr {
		s.stderr = append(s.stderr, line)
	} else {
		s.stdout = append(s.stdout, line)
	}

	// The notification has its own much smaller budget, so it tracks bytes
	// rather than lines and stops growing once full.
	if s.notifBytes < streamNotifMaxBytes {
		s.notif = append(s.notif, line)
		s.notifBytes += len(line) + 1
	}
}

// summary renders the notification transcript, stdout and stderr interleaved
// in arrival order.
func (s *outputStream) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := strings.Join(s.notif, "\n")
	if s.dropped {
		text += "\n...[output truncated]"
	}
	return text
}

// pending reports whether anything is buffered, so the caller can skip
// sending an empty batch.
func (s *outputStream) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stdout) > 0 || len(s.stderr) > 0
}

// take swaps out the buffered lines, leaving empty slices behind. Both keys
// are always present in the packet even when one is empty.
func (s *outputStream) take() (stdout, stderr []string, dropped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stdout, stderr, dropped = s.stdout, s.stderr, s.dropped
	s.stdout, s.stderr = nil, nil
	return stdout, stderr, dropped
}

// send emits one commandOutput packet.
func (s *outputStream) send(stdout, stderr []string, dropped bool) {
	if len(stdout) == 0 {
		stdout = []string{}
	}
	if len(stderr) == 0 {
		stderr = []string{}
	}
	if dropped {
		stderr = append(stderr, "[output truncated: over 2000 lines]")
	}

	body := map[string]any{
		"commandOutput": true,
		"id":            s.id,
		"stdout":        stdout,
		"stderr":        stderr,
	}
	pkt, err := protocol.NewPacket(PacketTypeOutput, body)
	if err != nil {
		s.logger.Warn("runcommand: build output packet", log.Error(err))
		return
	}
	if err := s.dev.Send(pkt); err != nil {
		s.logger.Warn("runcommand: send output", log.Error(err), log.Int("id", int(s.id)))
	}
}

// flush sends a batch only when lines are buffered, so an idle command does not
// generate traffic.
//
// The bus copy is gated on subscribers. A chatty command would otherwise
// publish four events a second to nobody, and the bus drops events (with a
// warning) once a slow subscriber fills its channel -- so an unfiltered feed
// would both waste work and make the drop warnings fire for output nobody
// asked for. The lifecycle events are unconditional, so a client can still
// learn the result of a command it was not watching line by line.
func (s *outputStream) flush() {
	if !s.pending() {
		return
	}
	stdout, stderr, dropped := s.take()
	s.send(stdout, stderr, dropped)

	if s.bus == nil || !s.bus.HasSubscribers(events.TypeRunCommandOutput) {
		return
	}
	s.bus.Publish(events.TypeRunCommandOutput, s.dev.ID(), map[string]any{
		"id":        s.id,
		"key":       s.command,
		"status":    "output",
		"stdout":    stdout,
		"stderr":    stderr,
		"truncated": dropped,
	})
}

// publishLifecycle announces a start or finish. Unconditional: these are two
// events per command and carry the result a client needs most.
func (s *outputStream) publishLifecycle(status string, extra map[string]any) {
	if s.bus == nil {
		return
	}
	payload := map[string]any{"id": s.id, "key": s.command, "status": status}
	for k, v := range extra {
		payload[k] = v
	}
	s.bus.Publish(events.TypeRunCommandOutput, s.dev.ID(), payload)
}

// line is one scanned line tagged with the stream it came from.
type line struct {
	text     string
	isStderr bool
}

// streamOutput runs cmd and reports its output to the phone as it arrives.
// The three packet types must be sent in order -- started, then output, then
// finished -- because the phone keys its output rows off the id registered by
// commandStarted; a finished packet for an unknown id only flips its spinner.
//
// sendFinished is deferred so every exit path, including a panic, still
// releases the phone's running indicator.
func (p *RunCommandPlugin) streamOutput(
	ctx context.Context,
	dev device.Sender,
	id int32,
	key string,
	cmd *exec.Cmd,
) string {
	var stream *outputStream
	// The finish packet reports the outcome, so it is always sent exactly
	// once, including on the early-return paths below.
	var success bool
	// summary is filled in on the normal exit path and read by the deferred
	// publish, so the transcript is rendered once rather than twice.
	var summary string
	defer func() {
		if stream != nil && stream.stopped {
			success = false
		}
		// Only for an execution that actually started, so a client never sees
		// a finish with no matching start.
		if stream != nil && stream.announced {
			stream.publishLifecycle("finished", map[string]any{
				"success": success,
				"output":  summary,
			})
		}
		p.finishOutput(dev, id, success)
	}()

	// Built before the pipes exist so the deferred finish can still find it on
	// the early-return paths; announced stays false until the process is
	// actually running, so a failed start publishes neither half.
	stream = &outputStream{dev: dev, logger: p.logger, bus: p.bus, id: id, command: key}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		p.logger.Warn("runcommand: stdout pipe", log.Error(err))
		return ""
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		p.logger.Warn("runcommand: stderr pipe", log.Error(err))
		return ""
	}
	if err := cmd.Start(); err != nil {
		p.logger.Warn("runcommand: start", log.Error(err))
		return ""
	}
	stream.announced = true
	stream.publishLifecycle("started", nil)

	// exec.CommandContext kills only the direct child. If the shell forks
	// rather than execs, the orphan keeps the write end of the pipes open, so
	// the scanners would never see EOF and Wait would never be reached --
	// cancelling would hang instead of stopping. Closing our read ends when
	// the context ends makes that unblock deterministically, whatever the
	// shell does.
	scanDone := make(chan struct{})
	defer close(scanDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = stdoutPipe.Close()
			_ = stderrPipe.Close()
		case <-scanDone:
		}
	}()

	lines := make(chan line, streamChanBuffer)
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); scanInto(lines, stdoutPipe, false, p.logger) }()
	go func() { defer readers.Done(); scanInto(lines, stderrPipe, true, p.logger) }()
	go func() {
		readers.Wait()
		close(lines)
	}()

	ticker := time.NewTicker(streamFlushInterval)
	defer ticker.Stop()

	// The loop runs until both pipes hit EOF rather than returning on ctx.
	// Cancelling the context kills the process, which closes the pipes, which
	// ends the scanners. Tailing the channel to completion keeps the scanner
	// goroutines from blocking forever on a send nobody is reading, and lets
	// the final flush carry whatever output arrived before the kill.
	for {
		closed := drainLines(lines, stream)
		stream.flush()
		if closed {
			break
		}

		select {
		case l, ok := <-lines:
			if ok {
				stream.add(l.isStderr, l.text)
			}
		case <-ticker.C:
			stream.flush()
		case <-ctx.Done():
			// The process is being killed; keep draining until EOF so the
			// last lines are not lost, then report failure.
			stream.stopped = true
		}
	}

	readers.Wait()
	waitErr := cmd.Wait()
	success = waitErr == nil

	summary = stream.summary()
	return summary
}

// drainLines consumes everything currently buffered and reports whether the
// channel has closed and gone empty. Draining first is what collapses a burst
// into one packet instead of one packet per line.
func drainLines(lines <-chan line, s *outputStream) (closed bool) {
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return true
			}
			s.add(l.isStderr, l.text)
		default:
			return false
		}
	}
}

// scanInto splits a pipe into lines. Sends block so a slow consumer applies
// backpressure to the command rather than silently discarding output; the
// line cap in add is the authoritative bound.
func scanInto(out chan<- line, r io.Reader, isStderr bool, logger log.Logger) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), streamMaxScanBuffer)
	for scanner.Scan() {
		out <- line{text: scanner.Text(), isStderr: isStderr}
	}
	if err := scanner.Err(); err != nil {
		logger.Debug("runcommand: output scan ended", log.Error(err))
	}
}

// finishOutput emits commandFinished and clears the cancel registration.
// The phone uses this to turn the command line green or red, so it must be
// sent exactly once per started execution.
func (p *RunCommandPlugin) finishOutput(dev device.Sender, id int32, success bool) {
	p.Mu.Lock()
	if byID, ok := p.running[dev.ID()]; ok {
		delete(byID, id)
		if len(byID) == 0 {
			delete(p.running, dev.ID())
		}
	}
	p.Mu.Unlock()

	body := map[string]any{
		"commandFinished": true,
		"id":              id,
		"success":         success,
	}
	pkt, err := protocol.NewPacket(PacketTypeOutput, body)
	if err != nil {
		p.logger.Warn("runcommand: build finished packet", log.Error(err))
		return
	}
	if err := dev.Send(pkt); err != nil {
		p.logger.Warn("runcommand: send finished", log.Error(err), log.Int("id", int(id)))
	}
}

// nextExecID hands out execution ids. They must fit a Java int because the
// phone reads them with getInt; a nanosecond timestamp does not, so this is a
// plain counter.
func (p *RunCommandPlugin) nextExecID() int32 {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.execSeq++
	return p.execSeq
}

// stopRunning cancels a running execution, if the phone asked for it. The
// phone's stop button sends kdeconnect.runcommand.request {"stop": true}.
func (p *RunCommandPlugin) stopRunning(deviceID string, id int32) {
	p.Mu.Lock()
	cancel := p.running[deviceID][id]
	p.Mu.Unlock()

	if cancel == nil {
		p.logger.Debug("runcommand: stop for unknown execution",
			log.String("device_id", deviceID), log.Int("id", int(id)))
		return
	}
	p.logger.Info("runcommand: stopping execution on request",
		log.String("device_id", deviceID), log.Int("id", int(id)))
	cancel()
}

// stopAll cancels everything running for a device, used when it disconnects.
func (p *RunCommandPlugin) stopAll(deviceID string) {
	p.Mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(p.running[deviceID]))
	for _, cancel := range p.running[deviceID] {
		cancels = append(cancels, cancel)
	}
	delete(p.running, deviceID)
	p.Mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}
