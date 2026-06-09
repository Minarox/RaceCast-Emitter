package scripts

// StreamManagerOptions configures the StreamManager.
type StreamManagerOptions struct {
	NoCam     bool
	NoMic     bool
	Fake      bool
	RecordDir string // if set, each captured device is also recorded locally
}

// StreamManager wires a CaptureManager (GStreamer, no LiveKit dependency) to a
// PublishManager (LiveKit with automatic reconnection).
//
// Capture starts independently of LiveKit connectivity: if the LiveKit
// connection is lost, pipelines keep running and frames are re-published
// transparently when the connection is re-established.
type StreamManager struct {
	capture *CaptureManager
	publish *PublishManager
}

// NewStreamManager creates and wires a CaptureManager and a PublishManager.
func NewStreamManager(opts StreamManagerOptions) *StreamManager {
	cm := NewCaptureManager(CaptureOptions{
		NoCam:     opts.NoCam,
		NoMic:     opts.NoMic,
		Fake:      opts.Fake,
		RecordDir: opts.RecordDir,
	})
	pm := NewPublishManager()
	cm.OnStreamAdded(pm.Publish)
	cm.OnStreamRemoved(pm.Unpublish)
	return &StreamManager{capture: cm, publish: pm}
}

// Start launches the LiveKit publish loop and then starts the capture
// pipelines (and hot-plug watcher).
func (sm *StreamManager) Start() {
	sm.publish.Start()
	sm.capture.Start()
}

// Stop shuts down capture first, then the publish manager.
func (sm *StreamManager) Stop() {
	sm.capture.Stop()
	sm.publish.Stop()
}

