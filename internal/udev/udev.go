package udev

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"
)

// Event represents a kernel udev event received via netlink.
type Event struct {
	Action    string // "add", "remove", "change"
	Subsystem string // "video4linux", "sound", ...
}

// Listen opens a KOBJECT_UEVENT netlink socket and sends video and audio
// subsystem events on the returned channel.
// The channel is closed when ctx is cancelled.
func Listen(ctx context.Context) (<-chan Event, error) {
	fd, err := syscall.Socket(
		syscall.AF_NETLINK,
		// SOCK_NONBLOCK: non-blocking reads allow cancellation via ctx.
		syscall.SOCK_RAW|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK,
		syscall.NETLINK_KOBJECT_UEVENT,
	)
	if err != nil {
		return nil, fmt.Errorf("socket netlink : %w", err)
	}

	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Groups: 1, // group 1 = raw kernel events
	}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind netlink : %w", err)
	}

	ch := make(chan Event, 8)
	go func() {
		defer close(ch)
		defer syscall.Close(fd)

		buf := make([]byte, 8192)
		// idleTick avoids creating a new timer per wait cycle.
		idleTick := time.NewTicker(100 * time.Millisecond)
		defer idleTick.Stop()
		for {
			n, _, err := syscall.Recvfrom(fd, buf, 0)
			if err != nil {
				if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
					select {
					case <-ctx.Done():
						return
					case <-idleTick.C:
					}
					continue
				}
				return
			}

			ev := parse(buf[:n])
			if ev == nil {
				continue
			}
			// Filter on relevant subsystems only.
			if ev.Subsystem != "video4linux" && ev.Subsystem != "sound" {
				continue
			}
			select {
			case ch <- *ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// parse extracts the action and subsystem from a netlink uevent message.
// Format: null-separated strings: "action@devpath\0KEY=VALUE\0KEY=VALUE\0..."
func parse(data []byte) *Event {
	parts := strings.Split(string(data), "\x00")
	if len(parts) < 2 {
		return nil
	}
	ev := &Event{}
	for _, part := range parts {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch key {
		case "ACTION":
			ev.Action = val
		case "SUBSYSTEM":
			ev.Subsystem = val
		}
	}
	// Fallback: extract action from the first token "action@devpath".
	if ev.Action == "" {
		if idx := strings.Index(parts[0], "@"); idx >= 0 {
			ev.Action = parts[0][:idx]
		}
	}
	if ev.Action == "" {
		return nil
	}
	return ev
}
