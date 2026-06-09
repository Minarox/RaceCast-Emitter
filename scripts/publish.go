package scripts

import (
	"context"
	"sync"
	"time"

	"racecast-emitter/utils"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4/pkg/media"
)

// publishedStream tracks an active LiveKit publication for one capture stream.
type publishedStream struct {
	info   StreamInfo
	track  *lksdk.LocalSampleTrack
	pubSID string
	cancel context.CancelFunc
	done   chan struct{} // closed when the forwarder goroutine exits
}

// PublishManager subscribes to CaptureManager events and publishes encoded
// frames to a LiveKit room.  It reconnects automatically when the LiveKit
// connection is lost without interrupting the underlying capture pipelines.
//
// Thread-safe.
type PublishManager struct {
	mu              sync.Mutex
	room            *lksdk.Room
	capturedStreams  map[string]StreamInfo       // source of truth: all active captures
	publishedStreams map[string]*publishedStream // currently published to LiveKit
	stopCh          chan struct{}
	wg              sync.WaitGroup
}

// NewPublishManager creates a PublishManager ready to be started.
func NewPublishManager() *PublishManager {
	return &PublishManager{
		capturedStreams:  make(map[string]StreamInfo),
		publishedStreams: make(map[string]*publishedStream),
		stopCh:          make(chan struct{}),
	}
}

// Start launches the LiveKit connection loop in the background.
func (pm *PublishManager) Start() {
	pm.wg.Add(1)
	go pm.connectLoop()
}

// Stop cancels all forwarder goroutines, waits for them to exit, then
// disconnects from LiveKit.
func (pm *PublishManager) Stop() {
	pm.mu.Lock()
	for _, ps := range pm.publishedStreams {
		ps.cancel()
	}
	room := pm.room
	pm.room = nil
	pm.mu.Unlock()

	close(pm.stopCh)
	pm.wg.Wait()

	if room != nil {
		room.Disconnect()
	}
}

// Publish is called (by CaptureManager.OnStreamAdded) when a new capture
// stream is ready.  It is safe to call before LiveKit is connected.
func (pm *PublishManager) Publish(info StreamInfo) {
	pm.mu.Lock()
	pm.capturedStreams[info.Key] = info
	room := pm.room
	pm.mu.Unlock()

	if room != nil {
		pm.publishStream(room, info)
	}
	// If room is nil, connectLoop will publish this stream on next connection.
}

// Unpublish is called (by CaptureManager.OnStreamRemoved) when a capture
// stream stops.
func (pm *PublishManager) Unpublish(key string) {
	pm.mu.Lock()
	delete(pm.capturedStreams, key)
	ps, ok := pm.publishedStreams[key]
	if ok {
		delete(pm.publishedStreams, key)
	}
	room := pm.room
	pm.mu.Unlock()

	if !ok {
		return
	}
	ps.cancel()
	<-ps.done // wait for forwarder to exit before unpublishing

	if room != nil {
		if err := room.LocalParticipant.UnpublishTrack(ps.pubSID); err != nil {
			utils.Log.Warnw("Failed to unpublish track.", "key", key, "error", err)
		}
	}
	utils.Log.Infow("Stream unpublished.", "key", key)
}

// ---------------------------------------------------------------------------
// Connection loop
// ---------------------------------------------------------------------------

func (pm *PublishManager) connectLoop() {
	defer pm.wg.Done()

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-pm.stopCh:
			return
		default:
		}

		utils.Log.Infow("Connecting to LiveKit...")

		disconnected := make(chan struct{})
		cb := &lksdk.RoomCallback{
			OnDisconnected: func() { close(disconnected) },
		}

		room, err := utils.TryConnectToLiveKitRoom(cb)
		if err != nil {
			utils.Log.Warnw("LiveKit connection failed; retrying.", "error", err, "in", backoff)
			select {
			case <-pm.stopCh:
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = time.Second

		// Store room and publish all currently active captures.
		pm.mu.Lock()
		pm.room = room
		captured := make([]StreamInfo, 0, len(pm.capturedStreams))
		for _, info := range pm.capturedStreams {
			captured = append(captured, info)
		}
		pm.mu.Unlock()

		utils.Log.Infow("LiveKit connected; publishing active streams.", "count", len(captured))
		for _, info := range captured {
			pm.publishStream(room, info)
		}

		// Wait for disconnection or stop signal.
		select {
		case <-pm.stopCh:
			return
		case <-disconnected:
			utils.Log.Warnw("LiveKit disconnected; will reconnect.")
			pm.handleDisconnect()
		}
	}
}

// handleDisconnect cancels all forwarder goroutines and clears published state.
// The capturedStreams map is preserved so they can be re-published on reconnect.
func (pm *PublishManager) handleDisconnect() {
	pm.mu.Lock()
	toCancel := make([]*publishedStream, 0, len(pm.publishedStreams))
	for _, ps := range pm.publishedStreams {
		toCancel = append(toCancel, ps)
	}
	pm.publishedStreams = make(map[string]*publishedStream)
	pm.room = nil
	pm.mu.Unlock()

	for _, ps := range toCancel {
		ps.cancel()
		<-ps.done // wait for forwarder to exit cleanly
	}
}

// ---------------------------------------------------------------------------
// Stream publishing
// ---------------------------------------------------------------------------

func (pm *PublishManager) publishStream(room *lksdk.Room, info StreamInfo) {
	// Guard against starting a new forwarder while stopping.
	select {
	case <-pm.stopCh:
		return
	default:
	}

	var (
		track  *lksdk.LocalSampleTrack
		pubSID string
		err    error
	)

	switch info.Kind {
	case VideoStream:
		track, pubSID, err = utils.PublishVideoTrack(room, info.Name)
	case AudioStream:
		track, pubSID, err = utils.PublishAudioTrack(room, info.Name)
	}

	if err != nil {
		utils.Log.Errorw("Failed to publish stream to LiveKit; will retry on reconnect.",
			"key", info.Key, "error", err)
		return
	}

	// For video: request an IDR frame when a subscriber binds to the track.
	if info.Kind == VideoStream && info.Pipeline != nil {
		track.OnBind(func() { info.Pipeline.ForceKeyframe() })
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ps := &publishedStream{
		info:   info,
		track:  track,
		pubSID: pubSID,
		cancel: cancel,
		done:   done,
	}

	pm.mu.Lock()
	pm.publishedStreams[info.Key] = ps
	pm.mu.Unlock()

	pm.wg.Add(1)
	go pm.forward(ctx, done, track, info)

	utils.Log.Infow("Stream published.", "key", info.Key, "name", info.Name, "sid", pubSID)
}

// forward reads frames from the capture channel and writes them to the LiveKit
// track until either the context is cancelled or the capture channel is closed.
func (pm *PublishManager) forward(ctx context.Context, done chan struct{}, track *lksdk.LocalSampleTrack, info StreamInfo) {
	defer pm.wg.Done()
	defer close(done)

	var warned bool
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-info.Frames:
			if !ok {
				// Pipeline stopped — capture is gone; CaptureManager will
				// call Unpublish, which handles cleanup.
				return
			}
			if err := track.WriteSample(media.Sample{Data: frame.Data, Duration: frame.Duration}, nil); err != nil {
				if !warned {
					utils.Log.Warnw("Failed to write sample to LiveKit track.", "key", info.Key, "error", err)
					warned = true
				}
			}
		}
	}
}
