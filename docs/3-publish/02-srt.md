# SRT clients

|           | supported codecs                                      |
| --------- | ----------------------------------------------------- |
| **video** | H265, H264, MPEG-4 Video (H263, Xvid), MPEG-1/2 Video |
| **audio** | Opus, MPEG-4 Audio (AAC), MPEG-1/2 Audio (MP3), AC-3  |
| **other** | KLV                                                   |

SRT is a protocol that allows to publish and read live data stream, providing encryption, integrity and a retransmission mechanism. It is usually used to transfer media streams encoded with MPEG-TS. In order to publish a stream to the server with the SRT protocol, use this URL:

```
srt://localhost:8890?streamid=publish:mystream&pkt_size=1316
```

Replace `mystream` with any name you want. The resulting stream will be available on path `/mystream`.

If you need to use the standard stream ID syntax instead of the custom one in use by this server, read [Standard stream ID syntax](../2-features/23-srt-specific-features.md#standard-stream-id-syntax).

### Cellular or damaged links: missing tracks at startup

Cellular bonding links and damaged sources can send corrupted or incomplete MPEG-TS packets during the first seconds. MediaMTX can then detect only the tracks advertised by the first valid PMT (for instance H265) and discard later tracks (for instance AAC) as undeclared PIDs. Set `mpegtsAnalyzeDuration` on the path (or globally) to a positive duration to buffer and analyze the beginning of the stream before publishing it:

```yaml
paths:
  mystream:
    source: publisher
    mpegtsAnalyzeDuration: 2s
```

During this finite window, corrupted leading packets and incomplete early PMTs are skipped, and the most complete validated track set is published. The buffered amount is bounded, and window expiry, video-only streams, persistent corruption and source disconnection all produce a definite result. `0` (the default) keeps the original startup latency and behavior.

If you want to publish a stream by using a client in listening mode (i.e. with `mode=listener` appended to the URL), read the next section.

Some clients that can publish with SRT are [FFmpeg](08-ffmpeg.md), [GStreamer](09-gstreamer.md), [OBS Studio](10-obs-studio.md).
