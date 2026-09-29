//! Microphone samples for LiveKit: converted to 16-bit (what the SDK's Opus encoder takes) and fed in
//! 10 ms frames by a dedicated task. The capture thread only does a non-blocking `try_send`: the stream can
//! never slow down the recording.

use std::borrow::Cow;

use livekit::webrtc::audio_frame::AudioFrame;
use livekit::webrtc::audio_source::native::NativeAudioSource;
use tokio::sync::{mpsc, watch};

use crate::capture::modes::SampleFormat;

/// Converts interleaved little-endian PCM to 16-bit samples (keeps the most significant bits).
pub fn to_i16(pcm: &[u8], format: SampleFormat) -> Vec<i16> {
    match format {
        SampleFormat::S16 => pcm.as_chunks::<2>().0.iter().map(|&b| i16::from_le_bytes(b)).collect(),
        SampleFormat::S24 => pcm.as_chunks::<3>().0.iter().map(|&[_, lo, hi]| i16::from_le_bytes([lo, hi])).collect(),
        SampleFormat::S32 => {
            pcm.as_chunks::<4>().0.iter().map(|&[_, _, lo, hi]| i16::from_le_bytes([lo, hi])).collect()
        }
    }
}

/// Capture-side end: drops the samples while the track is not published or if the feeder lags.
pub struct AudioTap {
    tx: mpsc::Sender<Vec<i16>>,
    published: watch::Receiver<bool>,
    format: SampleFormat,
}

impl AudioTap {
    pub fn push(&self, pcm: &[u8]) {
        if *self.published.borrow() {
            let _ = self.tx.try_send(to_i16(pcm, self.format));
        }
    }
}

/// Creates the tap and spawns the feeding task, which stops when the tap is dropped.
pub fn spawn(
    source: NativeAudioSource,
    published: watch::Receiver<bool>,
    format: SampleFormat,
    sample_rate: u32,
    channels: u32,
) -> AudioTap {
    let (tx, mut rx) = mpsc::channel::<Vec<i16>>(64);
    let frame_len = (sample_rate / 100 * channels) as usize;
    tokio::spawn(async move {
        let mut pending: Vec<i16> = Vec::with_capacity(frame_len * 4);
        let mut warned = false;
        while let Some(chunk) = rx.recv().await {
            pending.extend_from_slice(&chunk);
            while pending.len() >= frame_len {
                let frame = AudioFrame {
                    data: Cow::Borrowed(&pending[..frame_len]),
                    sample_rate,
                    num_channels: channels,
                    samples_per_channel: sample_rate / 100,
                };
                if let Err(e) = source.capture_frame(&frame).await
                    && !std::mem::replace(&mut warned, true)
                {
                    tracing::warn!(error = %e, "audio frame refused by LiveKit");
                }
                pending.drain(..frame_len);
            }
        }
    });
    AudioTap { tx, published, format }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn conversions_keep_the_high_bits() {
        assert_eq!(to_i16(&[0x34, 0x12, 0xff, 0xff], SampleFormat::S16), [0x1234, -1]);
        assert_eq!(to_i16(&[0x56, 0x34, 0x12, 0x00, 0x00, 0x80], SampleFormat::S24), [0x1234, i16::MIN]);
        assert_eq!(to_i16(&[0x78, 0x56, 0x34, 0x12], SampleFormat::S32), [0x1234]);
        assert!(to_i16(&[1, 2, 3], SampleFormat::S16).len() == 1);
    }
}
