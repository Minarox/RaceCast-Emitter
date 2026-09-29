//! Broadcast WAV writer: `bext` chunk (`TimeReference`) + `iXML` chunk for timecode sync in DaVinci
//! Resolve, PCM data appended as it arrives.
//!
//! Crash resilience: the sizes in the header are rewritten about every second, so a file cut by a power loss
//! is readable up to the last update. Continuous recording can exceed the 4 GB RIFF limit (~4 h in 24-bit
//! stereo): a `JUNK` chunk reserved right after the RIFF header turns the file into RF64 (`ds64` chunk with
//! 64-bit sizes, EBU Tech 3306) when needed.

use std::fs::File;
use std::io::{self, BufWriter, Write};
use std::os::unix::fs::FileExt;
use std::path::Path;

use chrono::{DateTime, Local};

/// Size of the `ds64` payload: RIFF size, data size, sample count (u64 each) + table length (u32).
const DS64_LEN: u32 = 28;
const BEXT_LEN: u32 = 602;
const RF64_THRESHOLD: u64 = u32::MAX as u64;

pub struct BwfSpec<'a> {
    /// Device name, written in the description and in the iXML `TAPE` field.
    pub name: &'a str,
    pub sample_rate: u32,
    pub channels: u16,
    pub bits: u16,
    /// Samples since local midnight of the first sample.
    pub time_reference: u64,
    pub origination: DateTime<Local>,
    pub timecode_rate: u32,
}

pub struct BwfWriter {
    out: BufWriter<File>,
    ds64_offset: u64,
    data_offset: u64,
    data_len: u64,
    block_align: u64,
    header_every: u64,
    since_header: u64,
    rf64_threshold: u64,
}

impl BwfWriter {
    pub fn create(path: &Path, spec: &BwfSpec<'_>) -> io::Result<Self> {
        Self::create_with_threshold(path, spec, RF64_THRESHOLD)
    }

    fn create_with_threshold(path: &Path, spec: &BwfSpec<'_>, rf64_threshold: u64) -> io::Result<Self> {
        let block_align = u64::from(spec.channels) * u64::from(spec.bits / 8);
        let byte_rate = u64::from(spec.sample_rate) * block_align;
        let mut header = Vec::with_capacity(2048);
        header.extend_from_slice(b"RIFF\0\0\0\0WAVE");
        let ds64_offset = header.len() as u64;
        push_chunk(&mut header, b"JUNK", &[0; DS64_LEN as usize]);
        push_chunk(&mut header, b"bext", &bext(spec));
        push_chunk(&mut header, b"fmt ", &fmt(spec, byte_rate, block_align));
        push_chunk(&mut header, b"iXML", ixml(spec).as_bytes());
        header.extend_from_slice(b"data\0\0\0\0");
        let data_offset = header.len() as u64;

        let mut out = BufWriter::with_capacity(256 * 1024, File::create(path)?);
        out.write_all(&header)?;
        let mut writer = Self {
            out,
            ds64_offset,
            data_offset,
            data_len: 0,
            block_align: block_align.max(1),
            header_every: byte_rate.max(1),
            since_header: 0,
            rf64_threshold,
        };
        writer.update_header(0)?;
        Ok(writer)
    }

    /// Appends PCM samples (interleaved, little-endian, in the format given at creation).
    pub fn write(&mut self, pcm: &[u8]) -> io::Result<()> {
        self.out.write_all(pcm)?;
        self.data_len += pcm.len() as u64;
        self.since_header += pcm.len() as u64;
        if self.since_header >= self.header_every {
            self.since_header = 0;
            self.update_header(0)?;
        }
        Ok(())
    }

    /// Audio duration written so far.
    pub fn duration_s(&self) -> f64 {
        (self.data_len / self.block_align) as f64 * self.block_align as f64 / self.header_every as f64
    }

    /// Writes the final sizes and flushes the file to disk.
    pub fn finalize(mut self) -> io::Result<()> {
        let pad = self.data_len % 2;
        if pad == 1 {
            self.out.write_all(&[0])?;
        }
        self.update_header(pad)?;
        self.out.get_ref().sync_all()
    }

    fn update_header(&mut self, pad: u64) -> io::Result<()> {
        // Data first, so the header never claims more than what reached the file.
        self.out.flush()?;
        let file = self.out.get_ref();
        let riff_len = self.data_offset - 8 + self.data_len + pad;
        if riff_len > self.rf64_threshold {
            let mut ds64 = Vec::with_capacity(8 + DS64_LEN as usize);
            ds64.extend_from_slice(b"ds64");
            ds64.extend_from_slice(&DS64_LEN.to_le_bytes());
            ds64.extend_from_slice(&riff_len.to_le_bytes());
            ds64.extend_from_slice(&self.data_len.to_le_bytes());
            ds64.extend_from_slice(&(self.data_len / self.block_align).to_le_bytes());
            ds64.extend_from_slice(&0u32.to_le_bytes());
            file.write_all_at(&ds64, self.ds64_offset)?;
            file.write_all_at(&u32::MAX.to_le_bytes(), self.data_offset - 4)?;
            file.write_all_at(b"RF64\xff\xff\xff\xff", 0)
        } else {
            file.write_all_at(&(riff_len as u32).to_le_bytes(), 4)?;
            file.write_all_at(&(self.data_len as u32).to_le_bytes(), self.data_offset - 4)
        }
    }
}

fn push_chunk(buf: &mut Vec<u8>, id: &[u8; 4], payload: &[u8]) {
    buf.extend_from_slice(id);
    buf.extend_from_slice(&(payload.len() as u32).to_le_bytes());
    buf.extend_from_slice(payload);
    if payload.len() % 2 == 1 {
        buf.push(0);
    }
}

fn fixed(text: &str, len: usize) -> Vec<u8> {
    let mut v: Vec<u8> = text.bytes().take(len).collect();
    v.resize(len, 0);
    v
}

/// EBU Tech 3285 `bext` chunk, version 1 (602 bytes).
fn bext(spec: &BwfSpec<'_>) -> Vec<u8> {
    let mut b = Vec::with_capacity(BEXT_LEN as usize);
    b.extend(fixed(spec.name, 256)); // Description
    b.extend(fixed("RaceCast-Emitter", 32)); // Originator
    b.extend(fixed(spec.name, 32)); // OriginatorReference
    b.extend(fixed(&spec.origination.format("%Y-%m-%d").to_string(), 10));
    b.extend(fixed(&spec.origination.format("%H:%M:%S").to_string(), 8));
    b.extend_from_slice(&(spec.time_reference as u32).to_le_bytes()); // TimeReferenceLow
    b.extend_from_slice(&((spec.time_reference >> 32) as u32).to_le_bytes()); // TimeReferenceHigh
    b.extend_from_slice(&1u16.to_le_bytes()); // Version
    b.resize(BEXT_LEN as usize, 0); // UMID (64) + reserved (190)
    b
}

fn fmt(spec: &BwfSpec<'_>, byte_rate: u64, block_align: u64) -> Vec<u8> {
    let mut f = Vec::with_capacity(16);
    f.extend_from_slice(&1u16.to_le_bytes()); // WAVE_FORMAT_PCM
    f.extend_from_slice(&spec.channels.to_le_bytes());
    f.extend_from_slice(&spec.sample_rate.to_le_bytes());
    f.extend_from_slice(&(byte_rate as u32).to_le_bytes());
    f.extend_from_slice(&(block_align as u16).to_le_bytes());
    f.extend_from_slice(&spec.bits.to_le_bytes());
    f
}

fn ixml(spec: &BwfSpec<'_>) -> String {
    let (rate, tc) = (spec.sample_rate, spec.timecode_rate);
    let (hi, lo) = (spec.time_reference >> 32, spec.time_reference & 0xFFFF_FFFF);
    let tape: String = spec.name.chars().filter(|c| c.is_ascii_alphanumeric() || "-_".contains(*c)).collect();
    format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?><BWFXML><IXML_VERSION>1.61</IXML_VERSION>\
         <PROJECT>RaceCast</PROJECT><TAPE>{tape}</TAPE><SPEED><MASTER_SPEED>{tc}/1</MASTER_SPEED>\
         <CURRENT_SPEED>{tc}/1</CURRENT_SPEED><TIMECODE_RATE>{tc}/1</TIMECODE_RATE><TIMECODE_FLAG>NDF</TIMECODE_FLAG>\
         <FILE_SAMPLE_RATE>{rate}</FILE_SAMPLE_RATE><AUDIO_BIT_DEPTH>{}</AUDIO_BIT_DEPTH>\
         <TIMESTAMP_SAMPLES_SINCE_MIDNIGHT_HI>{hi}</TIMESTAMP_SAMPLES_SINCE_MIDNIGHT_HI>\
         <TIMESTAMP_SAMPLES_SINCE_MIDNIGHT_LO>{lo}</TIMESTAMP_SAMPLES_SINCE_MIDNIGHT_LO>\
         <TIMESTAMP_SAMPLE_RATE>{rate}</TIMESTAMP_SAMPLE_RATE></SPEED></BWFXML>",
        spec.bits
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    fn spec() -> BwfSpec<'static> {
        BwfSpec {
            name: "mic-test",
            sample_rate: 48_000,
            channels: 2,
            bits: 24,
            time_reference: 0x1_2345_6789,
            origination: Local.with_ymd_and_hms(2026, 9, 28, 13, 32, 1).unwrap(),
            timecode_rate: 30,
        }
    }

    /// `(id, declared size, payload offset)` of every chunk after the 12-byte header.
    fn chunks(d: &[u8]) -> Vec<(String, u32, usize)> {
        let mut out = Vec::new();
        let mut o = 12;
        while o + 8 <= d.len() {
            let id = String::from_utf8_lossy(&d[o..o + 4]).into_owned();
            let size = u32::from_le_bytes(d[o + 4..o + 8].try_into().unwrap());
            out.push((id, size, o + 8));
            if size == u32::MAX {
                break;
            }
            o += 8 + size as usize + (size as usize & 1);
        }
        out
    }

    fn temp(name: &str) -> std::path::PathBuf {
        std::env::temp_dir().join(format!("racecast-bwf-{}-{name}.wav", std::process::id()))
    }

    #[test]
    fn writes_a_valid_bwf() {
        let path = temp("plain");
        let mut w = BwfWriter::create(&path, &spec()).unwrap();
        w.write(&[1u8; 6 * 100]).unwrap();
        w.write(&[2u8; 3]).unwrap(); // odd length: padded on finalize
        w.finalize().unwrap();
        let d = std::fs::read(&path).unwrap();
        std::fs::remove_file(&path).unwrap();

        assert_eq!(&d[..4], b"RIFF");
        assert_eq!(u32::from_le_bytes(d[4..8].try_into().unwrap()) as usize, d.len() - 8);
        let c = chunks(&d);
        let ids: Vec<&str> = c.iter().map(|(id, ..)| id.as_str()).collect();
        assert_eq!(ids, ["JUNK", "bext", "fmt ", "iXML", "data"]);
        let (_, bext_len, bext_at) = c[1];
        assert_eq!(bext_len, BEXT_LEN);
        let tref_low = u32::from_le_bytes(d[bext_at + 338..bext_at + 342].try_into().unwrap());
        let tref_high = u32::from_le_bytes(d[bext_at + 342..bext_at + 346].try_into().unwrap());
        assert_eq!((u64::from(tref_high) << 32) | u64::from(tref_low), 0x1_2345_6789);
        assert_eq!(&d[bext_at + 320..bext_at + 338], b"2026-09-2813:32:01");
        let (_, _, fmt_at) = c[2];
        assert_eq!(u16::from_le_bytes(d[fmt_at + 12..fmt_at + 14].try_into().unwrap()), 6); // block align
        let (_, data_len, data_at) = c[4];
        assert_eq!(data_len, 603);
        assert_eq!(d.len(), data_at + 604);
    }

    #[test]
    fn header_is_valid_before_finalize() {
        let path = temp("crash");
        let mut w = BwfWriter::create(&path, &spec()).unwrap();
        w.write(&vec![0u8; 288_000 * 2]).unwrap(); // 2 s: the header was rewritten
        drop(w); // power cut: no finalize
        let d = std::fs::read(&path).unwrap();
        std::fs::remove_file(&path).unwrap();
        let (_, data_len, _) = chunks(&d)[4];
        assert_eq!(data_len, 288_000 * 2);
    }

    #[test]
    fn switches_to_rf64_beyond_the_limit() {
        let path = temp("rf64");
        let mut w = BwfWriter::create_with_threshold(&path, &spec(), 4096).unwrap();
        w.write(&[7u8; 6000]).unwrap();
        w.finalize().unwrap();
        let d = std::fs::read(&path).unwrap();
        std::fs::remove_file(&path).unwrap();

        assert_eq!(&d[..8], b"RF64\xff\xff\xff\xff");
        let c = chunks(&d);
        assert_eq!(c[0].0, "ds64");
        let at = c[0].2;
        let u64_at = |o: usize| u64::from_le_bytes(d[o..o + 8].try_into().unwrap());
        assert_eq!(u64_at(at) as usize, d.len() - 8);
        assert_eq!(u64_at(at + 8), 6000);
        assert_eq!(u64_at(at + 16), 1000);
        assert_eq!(c.last().map(|(id, size, _)| (id.as_str(), *size)), Some(("data", u32::MAX)));
    }
}
