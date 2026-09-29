//! Video frames for LiveKit: the NVMM surfaces of the camera's stream branch are handed to the SDK as
//! DMA-BUF file descriptors, so the SDK's Jetson AV1 encoder reads them without any copy. The SDK handles
//! keyframe requests (PLI) and congestion control itself.

use std::collections::VecDeque;
use std::sync::Mutex;

use gstreamer as gst;
use livekit::webrtc::prelude::{VideoFrame, VideoResolution, VideoRotation};
use livekit::webrtc::video_frame::native::{DmaBufPixelFormat, NativeBuffer};
use livekit::webrtc::video_source::native::NativeVideoSource;

/// Surfaces kept alive after being handed to the encoder, which reads them asynchronously. The stream
/// branch's `nvvidconv` pool must be larger than this plus the appsink queue.
const HELD_SURFACES: usize = 4;
/// Size of the `nvvidconv` output pool of the stream branch.
pub const POOL_SIZE: u32 = 10;

/// Leading fields of `NvBufSurface` (`nvbufsurface.h`, JetPack 7).
#[repr(C)]
struct NvBufSurface {
    gpu_id: u32,
    batch_size: u32,
    num_filled: u32,
    is_contiguous: bool,
    mem_type: i32,
    surface_list: *const NvBufSurfaceParams,
}

/// Leading fields of `NvBufSurfaceParams`.
#[repr(C)]
struct NvBufSurfaceParams {
    width: u32,
    height: u32,
    pitch: u32,
    color_format: i32,
    layout: i32,
    buffer_desc: u64,
}

// Offsets checked against the C header: a layout change must fail the build, not corrupt memory.
const _: () = assert!(std::mem::offset_of!(NvBufSurface, surface_list) == 24);
const _: () = assert!(std::mem::offset_of!(NvBufSurfaceParams, buffer_desc) == 24);

/// DMA-BUF file descriptor of the NVMM surface behind `buffer` (caps `video/x-raw(memory:NVMM)`).
fn dmabuf_fd(buffer: &gst::BufferRef) -> Option<i32> {
    let map = buffer.map_readable().ok()?;
    if map.size() < std::mem::size_of::<NvBufSurface>() {
        return None;
    }
    // SAFETY: mapping an NVMM buffer yields an `NvBufSurface` (size checked above) whose `surface_list`
    // points to `batch_size` parameter blocks, valid while the buffer is mapped.
    unsafe {
        let surface = std::ptr::read_unaligned(map.as_ptr().cast::<NvBufSurface>());
        if surface.surface_list.is_null() || surface.num_filled == 0 {
            return None;
        }
        let params = std::ptr::read_unaligned(surface.surface_list);
        i32::try_from(params.buffer_desc).ok().filter(|fd| *fd >= 0)
    }
}

/// Pushes the stream branch's NVMM frames into a LiveKit video source.
pub struct VideoFeed {
    source: NativeVideoSource,
    resolution: VideoResolution,
    held: Mutex<VecDeque<gst::Sample>>,
}

impl VideoFeed {
    pub fn new(source: NativeVideoSource, width: u32, height: u32) -> Self {
        Self { source, resolution: VideoResolution { width, height }, held: Mutex::new(VecDeque::new()) }
    }

    /// Hands one NVMM frame to the encoder. Returns `false` if the buffer is not an NVMM surface.
    pub fn push(&self, sample: gst::Sample) -> bool {
        let Some(fd) = sample.buffer().and_then(dmabuf_fd) else { return false };
        self.source.capture_frame(&VideoFrame {
            rotation: VideoRotation::VideoRotation0,
            timestamp_us: 0,
            buffer: NativeBuffer::from_dmabuf(fd, self.resolution.clone(), DmaBufPixelFormat::NV12),
            frame_metadata: None,
        });
        let mut held = self.held.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
        held.push_back(sample);
        while held.len() > HELD_SURFACES {
            held.pop_front();
        }
        true
    }

    /// Releases the surfaces still held (stream branch removed).
    pub fn release(&self) {
        self.held.lock().unwrap_or_else(std::sync::PoisonError::into_inner).clear();
    }
}

/// Stream size: the source aspect ratio fitted in the configured box (swapped for a portrait source),
/// dimensions rounded down to even numbers.
pub fn fit(src_w: u32, src_h: u32, box_w: u32, box_h: u32) -> (u32, u32) {
    let (box_w, box_h) = if src_h > src_w { (box_h, box_w) } else { (box_w, box_h) };
    let (sw, sh) = (u64::from(src_w.max(1)), u64::from(src_h.max(1)));
    let (bw, bh) = (u64::from(box_w), u64::from(box_h));
    let (w, h) = if bw * sh <= bh * sw { (bw, bw * sh / sw) } else { (bh * sw / sh, bh) };
    let even = |v: u64| u32::try_from(v & !1).unwrap_or(u32::MAX).max(2);
    // Never upscale.
    (even(w.min(sw)), even(h.min(sh)))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stream_size_keeps_the_aspect_ratio() {
        assert_eq!(fit(1920, 1080, 960, 540), (960, 540));
        assert_eq!(fit(1280, 720, 960, 540), (960, 540));
        assert_eq!(fit(640, 480, 960, 540), (640, 480)); // no upscale
        assert_eq!(fit(1440, 1080, 960, 540), (720, 540)); // 4:3 in a 16:9 box
        assert_eq!(fit(1080, 1920, 960, 540), (540, 960)); // portrait (rotated camera)
        assert_eq!(fit(1920, 1080, 1280, 720), (1280, 720));
        let (w, h) = fit(1366, 768, 960, 540);
        assert!(w % 2 == 0 && h % 2 == 0 && w <= 960 && h <= 540, "{w}x{h}");
    }
}
