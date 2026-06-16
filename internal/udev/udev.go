package udev

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"
)

// Event représente un événement kernel udev reçu via netlink.
type Event struct {
	Action    string // "add", "remove", "change"
	Subsystem string // "video4linux", "sound", ...
}

// Listen ouvre un socket netlink KOBJECT_UEVENT et envoie sur le canal retourné
// les événements des sous-systèmes vidéo et audio.
// Le canal est fermé à la fin de la goroutine quand ctx est annulé.
func Listen(ctx context.Context) (<-chan Event, error) {
	fd, err := syscall.Socket(
		syscall.AF_NETLINK,
		// SOCK_NONBLOCK : lecture non bloquante, permet l'annulation via ctx
		syscall.SOCK_RAW|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK,
		syscall.NETLINK_KOBJECT_UEVENT,
	)
	if err != nil {
		return nil, fmt.Errorf("socket netlink : %w", err)
	}

	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Groups: 1, // groupe 1 = événements kernel bruts
	}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind netlink : %w", err)
	}

	ch := make(chan Event, 8)
	go func() {
		defer close(ch)
		defer syscall.Close(fd)

		buf := make([]byte, 8192)
		for {
			n, _, err := syscall.Recvfrom(fd, buf, 0)
			if err != nil {
				if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
					// Pas d'événement disponible : on attend un peu avant de retenter
					select {
					case <-ctx.Done():
						return
					case <-time.After(100 * time.Millisecond):
					}
					continue
				}
				return
			}

			ev := parse(buf[:n])
			if ev == nil {
				continue
			}
			// Filtrer sur les sous-systèmes pertinents uniquement
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

// parse extrait l'action et le sous-système d'un message netlink uevent.
// Le format est une suite de chaînes séparées par des octets nuls :
//
//	"action@devpath\0KEY=VALUE\0KEY=VALUE\0..."
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
	// Repli : extraire l'action depuis le premier token "action@devpath"
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
