# Voice Timeline Renderer

Voice Timeline Renderer is a self-hosted Go service for turning validated PCM
WAV recordings into a crossfaded, peak-normalized mixdown. The service exposes
an idempotent HTTP surface, keeps media and metadata under one data directory,
and records every render transition.

A timeline may also declare delivery groups (for example vocals and music).
One render of such a timeline then produces a time-aligned master plus one
stem per group that share the master's normalization gain.

The implementation covers:

- 16-bit integer PCM WAV validation and signal measurement.
- Ordered timeline spans with source-frame selection, gain, and crossfade.
- Optional delivery groups that assign every span to a named stem.
- Linear sample-rate conversion and mono/stereo channel mapping.
- Equal-power neighboring-span crossfades inside one delivery group.
- Peak normalization into a requested dBFS target, shared by master and stems.
- Durable render states, receipts, and atomically committed output folders.

Automated tests are intentionally deferred. A later task stage will add unit,
integration, and end-to-end verification around the production interfaces.

## Requirements

- Go 1.23 or newer.
- A writable local directory for source media, timeline metadata, render jobs,
  output WAV files, and receipts.

No network service, database server, or third-party Go module is required.

## Build

```bash
go build ./...
```

## Run

```bash
go run ./cmd/voiceservice \
  -addr :8093 \
  -data ./var/voice-timeline
```

The health endpoint is `GET http://127.0.0.1:8093/healthz`.

Runtime settings are supplied through arguments:

- `-addr`: HTTP listen address, default `:8093`.
- `-data`: persistent data root, default `./var/voice-timeline`.

No environment variable is required. Do not put credentials in the repository;
the service does not consume any remote credential.

## HTTP interface

### Register a source

`PUT /v1/sources/{source_id}`

```json
{
  "name": "opening take",
  "wav_base64": "UklGR..."
}
```

The source ID is chosen by the caller and must be 3-64 characters using
letters, digits, underscore, or hyphen. Repeating the same ID with exactly the
same name and bytes is idempotent. Reusing it with different content returns
the `source_conflict` error.

Response:

```json
{
  "id": "opening_take_01",
  "name": "opening take",
  "digest": "b7c0...",
  "byte_count": 96044,
  "sample_rate": 48000,
  "channels": 1,
  "bits_per_sample": 16,
  "frame_count": 48000,
  "duration_ms": 1000,
  "peak_dbfs": -8.0,
  "rms_dbfs": -11.0,
  "created_at": "2026-11-09T12:00:00Z"
}
```

### Compose a timeline

`PUT /v1/timelines/{timeline_id}`

```json
{
  "name": "two part narration",
  "sample_rate": 48000,
  "output_channels": 2,
  "target_peak_dbfs": -1,
  "spans": [
    {
      "source_id": "opening_take_01",
      "source_start_frame": 0,
      "source_end_frame": 48000,
      "gain_db": -2,
      "crossfade_frames": 0
    },
    {
      "source_id": "closing_take_01",
      "source_start_frame": 0,
      "source_end_frame": 44100,
      "gain_db": 0,
      "crossfade_frames": 4800
    }
  ]
}
```

Spans are sequential. A later span overlaps its predecessor by
`crossfade_frames`; the overlap is mixed with equal-power weights. A crossfade
must fit within both neighboring spans.

### Declare delivery groups

Add `delivery_groups` to the timeline and a `group_id` to every span to
deliver stems alongside the master:

```json
{
  "name": "narration over score",
  "sample_rate": 48000,
  "output_channels": 2,
  "target_peak_dbfs": -1,
  "delivery_groups": [
    {"id": "vocals", "label": "narration"},
    {"id": "music", "label": "score"}
  ],
  "spans": [
    {
      "source_id": "opening_take_01",
      "source_start_frame": 0,
      "source_end_frame": 48000,
      "gain_db": -2,
      "crossfade_frames": 0,
      "group_id": "vocals"
    },
    {
      "source_id": "score_bed_01",
      "source_start_frame": 0,
      "source_end_frame": 44100,
      "gain_db": 0,
      "crossfade_frames": 4800,
      "group_id": "music"
    }
  ]
}
```

When groups are declared, every span must reference a declared group and every
group must be used by at least one span. Group identifiers are 1-64 characters
of letters, digits, underscore, or hyphen. A timeline without
`delivery_groups` renders exactly as before and delivers only the master.

### Start a render

`PUT /v1/renders/{render_id}`

```json
{
  "timeline_id": "two_part_narration_01"
}
```

The render runs through the durable states
`queued -> rendering -> finalizing -> completed`. A failed render records a
terminal `failed` state with a stable error code and does not expose a partial
output folder.

### Read resources

- `GET /v1/sources/{source_id}`
- `GET /v1/timelines/{timeline_id}`
- `GET /v1/renders/{render_id}`
- `GET /v1/renders/{render_id}/receipt`
- `GET /v1/renders/{render_id}/output`
- `GET /v1/renders/{render_id}/stems/{group_id}`

The output route returns `audio/wav`. The receipt records source digests, span
placement, channel mapping, measured peak and RMS amplitude, normalization
gain, output digest, and non-fatal warnings. Grouped renders add a `delivery`
section described below. The stem route returns the `audio/wav` stem of one
delivery group of a completed render; it answers `404 stem_not_found` for an
undeclared group and `409 render_not_completed` until the render completes.

## Stem delivery

A render of a grouped timeline publishes the master (`output.wav`) plus one
stem per delivery group (`stems/<group_id>.wav`), all with identical frame
counts so the deliveries are time-aligned. The delivery contract:

- **Shared gain.** The normalization gain is derived from the master peak and
  applied unchanged to the master and to every stem. Stems are never
  normalized individually, so their relative loudness matches the master. The
  receipt publishes the value once as `delivery.shared_gain_db` (identical to
  `normalization.applied_gain_db`).
- **Exact reconstruction.** The unquantized master buffer is accumulated as
  the sum of the unquantized stem buffers, so the stems add back up to the
  master production signal by construction
  (`delivery.reconstruction.rule = sum_of_unquantized_stems_equals_master`).
- **Explained 16-bit deviation.** Master and stems are quantized with the
  same deterministic rule (round half away from zero, no dither), documented
  under `delivery.quantization`. The per-sample deviation between the
  quantized master and the sum of the quantized stems is bounded by
  `delivery.reconstruction.max_deviation_lsb` LSB, which accounts for the
  quantization of every delivery.
- **Group-local fades.** Equal-power crossfades apply only between spans of
  the same group. At a cross-group junction both neighbors keep their full
  gain during the overlap, so one group's junction never fades another
  group's audio. Span placement and output duration are unaffected.
- **Atomic delivery.** The master, every stem, and the receipt are staged in
  a temporary directory and published with one rename. If any stem fails to
  write, the job fails and no partial delivery becomes visible.
- **Bounded memory.** Sources are decoded and prepared once for the whole
  render, and a single scratch buffer is reused for every group, so working
  memory does not grow with the number of delivery groups.

Each stem entry in the receipt lists its file, digest, frame count, peak and
RMS amplitude, contributing span indices, and source identifiers; the job
resource mirrors the same summary under `stems`.

## Data directory

```text
var/voice-timeline/
  sources/<source_id>/
    source.wav
    metadata.json
  timelines/<timeline_id>.json
  jobs/<render_id>.json
  renders/<render_id>/
    output.wav
    stems/<group_id>.wav
    receipt.json
  tmp/
```

Source and render folders are assembled in `tmp/` and moved into place with one
directory rename. Metadata updates use same-directory temporary files followed
by rename.

## Source layout

```text
cmd/voiceservice/       service entry point
cmd/timelinecheck/      workflow execution entry point
internal/httpapi/       routing, request limits, JSON errors, media response
internal/service/       workflow orchestration and idempotency
internal/domain/        entities, validation, errors, and render transitions
internal/audiocodec/    WAV handling, transforms, and signal measurement
internal/render/        timeline preparation, mixing, normalization, receipt
internal/storage/       atomic local repository
internal/checkflow/     bounded production workflow checks
```

## Workflow checks

Run each declared production check from the repository root:

```bash
go run ./cmd/timelinecheck -workflow ingest-audio-source
go run ./cmd/timelinecheck -workflow compose-timeline
go run ./cmd/timelinecheck -workflow render-mixdown
go run ./cmd/timelinecheck -workflow render-stem-delivery
```

Each check creates an isolated temporary data directory, starts the real HTTP
handler, sends generated WAV inputs, and verifies read-back or rendered
output. The stem delivery check additionally decodes the master and stems,
confirms the quantized stem sum reconstructs the master within the receipt's
stated quantization bound, probes same-group and cross-group junction
behavior, and verifies that a failed commit publishes nothing. The checks are
bounded and do not require network access.

## Deferred test boundary

This baseline contains no unit tests, test fixtures, browser checks, or
`test_command`. The downstream task stage owns test creation and red/green
verification. It should extend coverage around WAV chunk edge cases, timeline
crossfade bounds, failed job cleanup, concurrent idempotency, and HTTP error
mapping.
